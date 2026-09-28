// Package pgproxy is a Postgres wire-protocol router. Clients connect with
// database=dbname@branch; the proxy reads the startup message, resolves the
// branch to its backend address, rewrites the database param back to the
// real dbname, replays the startup to the branch backend, and then relays
// bytes transparently in both directions (SCRAM auth flows untouched).
package pgproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"golang.org/x/sync/errgroup"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// genericRouteRefusal is the single client-facing message for every routing
// failure (unknown branch, not-ready branch, any resolver error). It is
// deliberately uniform so an UNAUTHENTICATED client cannot enumerate branch
// names or distinguish branch state; the real reason is logged server-side.
const genericRouteRefusal = "pgoverlay: database not available"

// BranchResolver maps a branch name to the "host:port" address of its
// Postgres instance. Implementations must only resolve branches that can
// accept connections.
type BranchResolver interface {
	ResolveBranch(name string) (addr string, err error)
}

// RegistryResolver adapts the registry: only ready branches resolve.
type RegistryResolver struct {
	Reg *registry.Registry
}

func (r *RegistryResolver) ResolveBranch(name string) (string, error) {
	b, err := r.Reg.GetBranchByName(name)
	if err != nil {
		return "", err // registry.ErrNotFound for unknown names
	}
	if b.State != registry.BranchReady {
		return "", fmt.Errorf("branch is %s, not ready", b.State)
	}
	return net.JoinHostPort(b.Host, strconv.Itoa(b.Port)), nil
}

// DoS-hardening defaults. New() seeds the Proxy fields with these, and every
// field is also read through a use-site helper, so a zero field falls back to
// the default and a bare &Proxy{Resolver: r} is exactly as protected as New(r).
// Only the cap can be switched off, and only with a negative value.
const (
	defaultDialTimeout    = 5 * time.Second
	defaultStartupTimeout = 10 * time.Second // client must finish the startup phase within this
	defaultMaxConns       = 256              // cap on concurrently-handled connections
	defaultIdleTimeout    = 15 * time.Minute // relay closes after this long with no bytes either way
)

type Proxy struct {
	Resolver BranchResolver
	// DialTimeout bounds the backend dial. Defaults to 5s.
	DialTimeout time.Duration
	// StartupTimeout bounds the entire startup phase (SSL/GSS negotiation +
	// StartupMessage). A client that connects and dribbles bytes — or sends
	// nothing — is dropped after this, freeing the goroutine+fd it pins.
	// Defaults to 10s; the deadline is cleared once relaying begins.
	StartupTimeout time.Duration
	// MaxConns caps the number of connections handled concurrently. When the
	// cap is reached, further accepts are refused fast (connection closed)
	// rather than queued unbounded. Defaults to 256; a negative value disables
	// the cap.
	MaxConns int
	// IdleTimeout closes a relayed session that has seen no bytes in either
	// direction for this long, reclaiming abandoned-but-open connections.
	// Defaults to 15m.
	IdleTimeout time.Duration
	// TLSConfig, when set, makes the proxy answer SSLRequest with 'S' and
	// upgrade the client connection via a server-side TLS handshake before
	// the startup message. When nil (default) SSLRequest is answered 'N' and
	// the session stays plaintext. Backend dials are always plaintext
	// (branches are local/cluster-internal).
	TLSConfig *tls.Config
}

func New(r BranchResolver) *Proxy {
	return &Proxy{
		Resolver:       r,
		DialTimeout:    defaultDialTimeout,
		StartupTimeout: defaultStartupTimeout,
		MaxConns:       defaultMaxConns,
		IdleTimeout:    defaultIdleTimeout,
	}
}

// durationOr returns d, or def when d is not positive.
func durationOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// capOr returns n, def when n is zero, and 0 ("no cap") when n is negative.
func capOr(n, def int) int {
	switch {
	case n > 0:
		return n
	case n == 0:
		return def
	default:
		return 0
	}
}

func (p *Proxy) dialTimeout() time.Duration {
	return durationOr(p.DialTimeout, defaultDialTimeout)
}

func (p *Proxy) startupTimeout() time.Duration {
	return durationOr(p.StartupTimeout, defaultStartupTimeout)
}

func (p *Proxy) idleTimeout() time.Duration {
	return durationOr(p.IdleTimeout, defaultIdleTimeout)
}

func (p *Proxy) maxConns() int { return capOr(p.MaxConns, defaultMaxConns) }

// Accept-retry backoff bounds, the same as net/http.Server's.
const (
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// Serve accepts connections until ctx is cancelled (which closes the
// listener) or Accept fails with a non-temporary error. Temporary Accept
// errors (EMFILE, ENFILE, ENOBUFS, ...) are retried with backoff, like
// net/http, so running out of file descriptors under a connection flood does
// not stop the router. A non-temporary error means the listener itself is
// broken and is returned. Each connection is handled in its own goroutine.
func (p *Proxy) Serve(ctx context.Context, lis net.Listener) error {
	stop := context.AfterFunc(ctx, func() { lis.Close() })
	defer stop()
	// Size the connection-cap semaphore from MaxConns once, here, so callers
	// that set MaxConns after New() (the field is exported for exactly that)
	// still get the cap they asked for.
	var sem chan struct{}
	if n := p.maxConns(); n > 0 {
		sem = make(chan struct{}, n)
	}
	var backoff time.Duration
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // graceful shutdown
			}
			if !isTemporaryAcceptError(err) {
				return err
			}
			backoff = min(max(2*backoff, minAcceptBackoff), maxAcceptBackoff)
			slog.Warn("pgproxy: accept failed, retrying", "error", err, "backoff", backoff)
			t := time.NewTimer(backoff)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return nil
			}
			continue
		}
		backoff = 0
		// Connection cap: acquire a slot before spawning the handler. If the
		// cap is full, refuse fast (close the conn) rather than queueing — an
		// unbounded backlog is itself the DoS we're guarding against.
		if sem != nil {
			select {
			case sem <- struct{}{}:
			default:
				slog.Warn("pgproxy: connection cap reached, refusing", "max", cap(sem))
				conn.Close()
				continue
			}
		}
		go func() {
			if sem != nil {
				defer func() { <-sem }()
			}
			p.handleConn(conn)
		}()
	}
}

// isTemporaryAcceptError reports whether an Accept error is worth retrying:
// descriptor or buffer exhaustion, and anything whose Temporary method says
// so.
func isTemporaryAcceptError(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	for _, errno := range []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM} {
		if errors.Is(err, errno) {
			return true
		}
	}
	var t interface{ Temporary() bool }
	return errors.As(err, &t) && t.Temporary()
}

// handleConn drives the startup phase: answer SSLRequest ('S' + TLS upgrade
// when TLSConfig is set, else 'N'), drop CancelRequest silently, then route
// the StartupMessage.
func (p *Proxy) handleConn(client net.Conn) {
	defer func() { client.Close() }() // closure: client may be re-bound to the TLS conn
	// Bound the whole startup phase: a client that connects and then sends
	// nothing (or dribbles) is dropped at this deadline, freeing the
	// goroutine+fd it would otherwise pin forever. Cleared in route() once we
	// hand off to the relay.
	client.SetReadDeadline(time.Now().Add(p.startupTimeout()))
	inTLS := false
	for {
		code, payload, err := readStartupFrame(client)
		if err != nil {
			return
		}
		switch code {
		case cancelRequestCode:
			// No session map in P2; close silently per protocol (the server
			// never replies to a cancel request).
			return
		case sslRequestCode:
			if p.TLSConfig == nil {
				// No TLS configured: answer 'N'; the client proceeds in
				// plaintext with a regular StartupMessage or disconnects.
				if _, err := client.Write([]byte{'N'}); err != nil {
					return
				}
				continue
			}
			if inTLS {
				// A second SSLRequest inside the TLS session is a protocol
				// violation (matches the PG server's behavior).
				writeRefusal(client, "08P01", "pgoverlay: SSLRequest received after TLS was already established")
				return
			}
			if _, err := client.Write([]byte{'S'}); err != nil {
				return
			}
			tlsConn := tls.Server(client, p.TLSConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			client = tlsConn
			inTLS = true
			continue
		case gssEncRequestCode:
			// No GSS encryption: answer 'N' (with or without TLS).
			if _, err := client.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		}
		var startup pgproto3.StartupMessage
		if err := startup.Decode(payload); err != nil {
			writeRefusal(client, "08P01", "pgoverlay: "+err.Error()) // protocol_violation
			return
		}
		p.route(client, &startup)
		return
	}
}

// route resolves the branch from the database param, rewrites the startup
// message, dials the backend, and relays.
func (p *Proxy) route(client net.Conn, startup *pgproto3.StartupMessage) {
	db := startup.Parameters["database"]
	dbname, branch, ok := splitDatabase(db)
	if !ok {
		writeRefusal(client, "3D000", // invalid_catalog_name
			fmt.Sprintf("pgoverlay: connect with dbname@branch (got database %q)", db))
		return
	}
	addr, err := p.Resolver.ResolveBranch(branch)
	if err != nil {
		// Uniform refusal: unknown vs not-ready vs any other resolve error are
		// indistinguishable to the (unauthenticated) client. Real reason logged.
		slog.Warn("pgproxy: route refused", "branch", branch, "reason", "resolve", "error", err)
		writeRefusal(client, "3D000", genericRouteRefusal) // invalid_catalog_name
		return
	}
	startup.Parameters["database"] = dbname
	raw, err := startup.Encode(nil)
	if err != nil {
		writeRefusal(client, "08P01", "pgoverlay: "+err.Error())
		return
	}
	backend, err := net.DialTimeout("tcp", addr, p.dialTimeout())
	if err != nil {
		// A resolved-but-unreachable backend would otherwise confirm the branch
		// name and its (down) state — collapse it into the same generic refusal.
		slog.Warn("pgproxy: route refused", "branch", branch, "reason", "dial", "addr", addr, "error", err)
		writeRefusal(client, "3D000", genericRouteRefusal)
		return
	}
	defer backend.Close()
	if _, err := backend.Write(raw); err != nil {
		return
	}
	// Startup is done: drop the startup read deadline. The relay installs its
	// own idle deadlines from here on.
	client.SetReadDeadline(time.Time{})
	relay(client, backend, p.idleTimeout())
}

// relay copies bytes in both directions until both sides are done. Each
// direction propagates EOF with a half-close (CloseWrite) so in-flight data
// in the other direction can still drain.
//
// idle, when > 0, is an inactivity timeout: every read from either side bumps
// BOTH connections' read deadlines forward by idle, so a session with no bytes
// flowing in either direction for that long has its reads time out and the
// relay tears down. (Bumping both sides — not just the active one — means a
// busy direction keeps the quiet direction alive, so we only close truly idle
// sessions, never merely-one-directional ones.)
func relay(client, backend net.Conn, idle time.Duration) error {
	if idle > 0 {
		bump := func() {
			d := time.Now().Add(idle)
			client.SetReadDeadline(d)
			backend.SetReadDeadline(d)
		}
		bump()
	}
	g := new(errgroup.Group)
	g.Go(func() error { return halfCopy(backend, client, idle) })
	g.Go(func() error { return halfCopy(client, backend, idle) })
	return g.Wait()
}

// halfCopy copies src->dst. When idle > 0 each successful read bumps both
// connections' read deadlines (via an idleReader wrapping src), so the copy
// returns with a timeout error once the whole session goes quiet.
func halfCopy(dst, src net.Conn, idle time.Duration) error {
	var r io.Reader = src
	if idle > 0 {
		r = &idleReader{src: src, peer: dst, idle: idle}
	}
	_, err := io.Copy(dst, r)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
	return err
}

// idleReader bumps both connections' read deadlines on every successful read,
// so any activity in either direction keeps the whole session alive and a
// fully-quiet session times out after idle.
type idleReader struct {
	src  net.Conn
	peer net.Conn
	idle time.Duration
}

func (r *idleReader) Read(b []byte) (int, error) {
	n, err := r.src.Read(b)
	if n > 0 {
		d := time.Now().Add(r.idle)
		r.src.SetReadDeadline(d)
		r.peer.SetReadDeadline(d)
	}
	return n, err
}
