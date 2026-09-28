// Package pgproxy is a Postgres wire-protocol router. Clients connect with
// database=dbname@branch; the proxy reads the startup message, resolves the
// branch to its backend address, rewrites the database param back to the
// real dbname, replays the startup to the branch backend, and then relays
// bytes transparently in both directions (SCRAM auth flows untouched).
//
// While relaying the backend's startup response the proxy watches the
// message frames (never altering them) for BackendKeyData, so a later
// CancelRequest carrying that key can be forwarded to the right backend.
package pgproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// genericRouteRefusal is the single client-facing message for every routing
// failure (unknown branch, not-ready branch, unreachable backend, any resolver
// error), so an UNAUTHENTICATED client cannot tell those states apart; the
// real reason is logged server-side.
//
// This narrows branch-name enumeration, it does not remove it. For a READY
// branch the proxy relays the backend's authentication challenge before any
// credential is checked, so a client can still confirm that a ready branch
// name exists (Postgres itself authenticates before it looks at the database;
// the proxy cannot do that without taking part in authentication). A branch
// that resolves but whose backend silently drops packets is refused only after
// DialTimeout, which is also observable. MaxStartupsPerIP bounds how many such
// probes one address can run at once; it does not rate-limit them.
const genericRouteRefusal = "pgoverlay: database not available"

// BranchResolver maps a branch name to the "host:port" address of its
// Postgres instance. Implementations must only resolve branches that can
// accept connections.
type BranchResolver interface {
	ResolveBranch(name string) (addr string, err error)
}

// BranchRefresher is optionally implemented by a BranchResolver that can
// re-read a branch's address from the runtime. When the dial to the resolved
// address fails, the proxy asks it once and, if the address moved (a pod that
// came back with a new IP, a container re-published on another port), dials
// the new address instead of refusing until the next reconcile pass repairs
// the registry.
type BranchRefresher interface {
	RefreshBranch(ctx context.Context, name string) (addr string, err error)
}

// RegistryResolver adapts the registry: only ready branches resolve.
type RegistryResolver struct {
	Reg *registry.Registry
	// Refresh, when set, re-reads a ready branch's address from the runtime
	// and records it if it moved (branchd wires engine.RefreshBranchEndpoint).
	// nil disables the refresh after a failed dial.
	Refresh func(ctx context.Context, name string) (string, error)
}

// RefreshBranch implements BranchRefresher through the Refresh hook.
func (r *RegistryResolver) RefreshBranch(ctx context.Context, name string) (string, error) {
	if r.Refresh == nil {
		return "", errors.New("no address refresh configured")
	}
	return r.Refresh(ctx, name)
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
// Only the two caps can be switched off, and only with a negative value.
const (
	defaultDialTimeout      = 5 * time.Second
	defaultFirstByteTimeout = 2 * time.Second  // client must send its first byte within this
	defaultStartupTimeout   = 10 * time.Second // client must finish the startup packets within this
	defaultAuthTimeout      = 30 * time.Second // backend must reach ReadyForQuery within this
	defaultMaxConns         = 256              // cap on concurrently-handled connections
	defaultMaxStartupsPerIP = 64               // cap on one client IP's connections still in startup
	defaultIdleTimeout      = 15 * time.Minute // relay closes after this long with no bytes either way
	defaultCloseGrace       = 5 * time.Second  // once one relay direction ends, the other gets this long
)

type Proxy struct {
	Resolver BranchResolver
	// DialTimeout bounds the backend dial (and a forwarded cancel request's
	// exchange with the backend). Defaults to 5s.
	DialTimeout time.Duration
	// FirstByteTimeout bounds how long a new connection may stay silent before
	// sending its first byte. Real clients write immediately after connecting,
	// so this is short: it keeps idle sockets from holding a connection slot
	// for the whole StartupTimeout. Defaults to 2s.
	FirstByteTimeout time.Duration
	// StartupTimeout bounds the client's startup packets (SSL/GSS negotiation,
	// the TLS handshake and the StartupMessage), measured from accept. A client
	// that dribbles bytes is dropped after this. Defaults to 10s.
	StartupTimeout time.Duration
	// AuthTimeout bounds the rest of the startup exchange: from routing the
	// StartupMessage until the backend sends its first ReadyForQuery, which
	// covers the whole authentication exchange. The proxy enforces it itself
	// instead of relying on the branch's authentication_timeout. Defaults to
	// 30s.
	AuthTimeout time.Duration
	// MaxConns caps the number of connections handled concurrently. When the
	// cap is reached, further accepts are refused fast (connection closed)
	// rather than queued unbounded. Defaults to 256; a negative value disables
	// the cap.
	MaxConns int
	// MaxStartupsPerIP caps how many connections from one client IP may be in
	// the startup phase at once (anything before the backend's first
	// ReadyForQuery: TLS, StartupMessage, authentication, and cancel
	// requests). Authenticated sessions do not count, so one host running many
	// real sessions is not limited, but one address cannot fill MaxConns with
	// connections that never authenticate. Over the cap, new connections are
	// refused fast. Defaults to 64; a negative value disables the cap.
	MaxStartupsPerIP int
	// IdleTimeout closes an authenticated session that has seen no bytes in
	// either direction for this long, reclaiming abandoned-but-open
	// connections. It bounds reads and writes, so a peer that stops reading
	// cannot pin the session either. Defaults to 15m.
	IdleTimeout time.Duration
	// TLSConfig, when set, makes the proxy answer SSLRequest with 'S' and
	// upgrade the client connection via a server-side TLS handshake before
	// the startup message. When nil (default) SSLRequest is answered 'N' and
	// the session stays plaintext. Backend dials are always plaintext
	// (branches are local/cluster-internal).
	TLSConfig *tls.Config

	// closeGrace overrides defaultCloseGrace (tests only).
	closeGrace time.Duration

	// cancels maps live sessions' cancel keys to their backend addresses.
	cancels cancelMap

	mu       sync.Mutex
	startups map[string]int // client IP -> connections still in startup
}

func New(r BranchResolver) *Proxy {
	return &Proxy{
		Resolver:         r,
		DialTimeout:      defaultDialTimeout,
		FirstByteTimeout: defaultFirstByteTimeout,
		StartupTimeout:   defaultStartupTimeout,
		AuthTimeout:      defaultAuthTimeout,
		MaxConns:         defaultMaxConns,
		MaxStartupsPerIP: defaultMaxStartupsPerIP,
		IdleTimeout:      defaultIdleTimeout,
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

func (p *Proxy) firstByteTimeout() time.Duration {
	return durationOr(p.FirstByteTimeout, defaultFirstByteTimeout)
}

func (p *Proxy) startupTimeout() time.Duration {
	return durationOr(p.StartupTimeout, defaultStartupTimeout)
}

func (p *Proxy) authTimeout() time.Duration {
	return durationOr(p.AuthTimeout, defaultAuthTimeout)
}

func (p *Proxy) idleTimeout() time.Duration {
	return durationOr(p.IdleTimeout, defaultIdleTimeout)
}

func (p *Proxy) graceTimeout() time.Duration {
	return durationOr(p.closeGrace, defaultCloseGrace)
}

func (p *Proxy) maxConns() int { return capOr(p.MaxConns, defaultMaxConns) }

func (p *Proxy) maxStartupsPerIP() int { return capOr(p.MaxStartupsPerIP, defaultMaxStartupsPerIP) }

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
		startupDone, ok := p.acquireStartupSlot(conn.RemoteAddr())
		if !ok {
			slog.Warn("pgproxy: per-IP startup cap reached, refusing",
				"client", conn.RemoteAddr().String(), "max", p.maxStartupsPerIP())
			conn.Close()
			if sem != nil {
				<-sem
			}
			continue
		}
		go func() {
			if sem != nil {
				defer func() { <-sem }()
			}
			defer startupDone()
			p.handleConn(conn, startupDone)
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

// acquireStartupSlot takes one of the client IP's startup slots. The returned
// release func is idempotent; it runs when the session reaches ReadyForQuery
// or, at the latest, when the connection ends. ok is false when the IP is at
// MaxStartupsPerIP.
func (p *Proxy) acquireStartupSlot(addr net.Addr) (release func(), ok bool) {
	limit := p.maxStartupsPerIP()
	tcp, isTCP := addr.(*net.TCPAddr)
	if limit <= 0 || !isTCP {
		return func() {}, true
	}
	ip := tcp.IP.String()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startups[ip] >= limit {
		return nil, false
	}
	if p.startups == nil {
		p.startups = make(map[string]int)
	}
	p.startups[ip]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.startups[ip]--; p.startups[ip] <= 0 {
				delete(p.startups, ip)
			}
		})
	}, true
}

// handleConn drives the startup phase: answer SSLRequest ('S' + TLS upgrade
// when TLSConfig is set, else 'N'), forward a CancelRequest whose key belongs
// to a live session, then route the StartupMessage. startupDone releases the
// client IP's startup slot once the session authenticates.
func (p *Proxy) handleConn(client net.Conn, startupDone func()) {
	raw := client
	defer func() { client.Close() }() // closure: client may be re-bound to the TLS conn
	// A client gets FirstByteTimeout to say anything at all, then the rest of
	// StartupTimeout (measured from accept) to finish its startup packets.
	// Both deadlines cover writes too, so a client that stops reading cannot
	// block our 'S'/'N' answers or the TLS handshake. route() replaces them.
	startDeadline := time.Now().Add(p.startupTimeout())
	client.SetDeadline(earliest(time.Now().Add(p.firstByteTimeout()), startDeadline))
	var first [1]byte
	if _, err := io.ReadFull(client, first[:]); err != nil {
		return
	}
	client.SetDeadline(startDeadline)
	// The first frame's leading byte is already consumed; readStartupFrame
	// never reads past a frame, so after that frame `in` reads client directly.
	in := io.MultiReader(bytes.NewReader(first[:]), client)
	inTLS := false
	for {
		code, payload, err := readStartupFrame(in)
		if err != nil {
			return
		}
		switch code {
		case cancelRequestCode:
			// The server never replies to a cancel request; the client learns
			// it was handled when the connection closes.
			p.forwardCancel(payload)
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
			in = tlsConn
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
		p.route(client, raw, &startup, startupDone)
		return
	}
}

func earliest(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// route resolves the branch from the database param, rewrites the startup
// message, dials the backend, and relays. raw is the client's underlying TCP
// connection (the same as client unless TLS is in use).
func (p *Proxy) route(client, raw net.Conn, startup *pgproto3.StartupMessage, startupDone func()) {
	// The client's startup packets are in. From here until the backend's
	// first ReadyForQuery (routing, the backend dial, authentication) both
	// sides must finish by authDeadline.
	authDeadline := time.Now().Add(p.authTimeout())
	client.SetDeadline(authDeadline)
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
	rawStartup, err := startup.Encode(nil)
	if err != nil {
		writeRefusal(client, "08P01", "pgoverlay: "+err.Error())
		return
	}
	backend, err := net.DialTimeout("tcp", addr, p.dialTimeout())
	if err != nil {
		// the branch may have moved (container recreated on a new port, pod
		// on a new IP): ask the resolver once and dial where it is now
		backend, addr, err = p.redial(branch, addr, err)
	}
	if err != nil {
		// A resolved-but-unreachable backend would otherwise confirm the branch
		// name and its (down) state — collapse it into the same generic refusal.
		slog.Warn("pgproxy: route refused", "branch", branch, "reason", "dial", "addr", addr, "error", err)
		writeRefusal(client, "3D000", genericRouteRefusal)
		return
	}
	defer backend.Close()
	backend.SetDeadline(authDeadline)
	if _, err := backend.Write(rawStartup); err != nil {
		return
	}
	s := &session{
		client:    client,
		clientRaw: raw,
		backend:   backend,
		addr:      addr,
		idle:      p.idleTimeout(),
		grace:     p.graceTimeout(),
		cancels:   &p.cancels,
		onReady:   startupDone,
	}
	s.relay()
}

// redial runs after the dial to a branch's resolved address failed: when the
// resolver can refresh addresses and the branch moved, it dials the new
// address once and returns it (the session records it for CancelRequest
// routing). Otherwise it returns the original dial error.
func (p *Proxy) redial(branch, addr string, dialErr error) (net.Conn, string, error) {
	rf, ok := p.Resolver.(BranchRefresher)
	if !ok {
		return nil, addr, dialErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.dialTimeout())
	defer cancel()
	moved, err := rf.RefreshBranch(ctx, branch)
	if err != nil || moved == "" || moved == addr {
		return nil, addr, dialErr
	}
	slog.Info("pgproxy: branch address moved; dialing the new one", "branch", branch, "from", addr, "to", moved)
	conn, err := net.DialTimeout("tcp", moved, p.dialTimeout())
	return conn, moved, err
}
