// Package pgproxy is a Postgres wire-protocol router. Clients connect with
// database=dbname@branch; the proxy reads the startup message, resolves the
// branch to its backend address, rewrites the database param back to the
// real dbname, replays the startup to the branch backend, and then relays
// bytes transparently in both directions (SCRAM auth flows untouched).
package pgproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"golang.org/x/sync/errgroup"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

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

type Proxy struct {
	Resolver BranchResolver
	// DialTimeout bounds the backend dial. Defaults to 5s.
	DialTimeout time.Duration
}

func New(r BranchResolver) *Proxy {
	return &Proxy{Resolver: r, DialTimeout: 5 * time.Second}
}

// Serve accepts connections until ctx is cancelled (which closes the
// listener) or Accept fails. Each connection is handled in its own goroutine.
func (p *Proxy) Serve(ctx context.Context, lis net.Listener) error {
	stop := context.AfterFunc(ctx, func() { lis.Close() })
	defer stop()
	for {
		conn, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil // graceful shutdown
			}
			return err
		}
		go p.handleConn(conn)
	}
}

// handleConn drives the startup phase: answer SSLRequest with 'N', drop
// CancelRequest silently, then route the StartupMessage.
func (p *Proxy) handleConn(client net.Conn) {
	defer client.Close()
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
		case sslRequestCode, gssEncRequestCode:
			// No TLS/GSS in P2: answer 'N'; the client proceeds in
			// plaintext with a regular StartupMessage or disconnects.
			if _, err := client.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		}
		var startup pgproto3.StartupMessage
		if err := startup.Decode(payload); err != nil {
			writeRefusal(client, "08P01", "pgbranch: "+err.Error()) // protocol_violation
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
			fmt.Sprintf("pgbranch: connect with dbname@branch (got database %q)", db))
		return
	}
	addr, err := p.Resolver.ResolveBranch(branch)
	if err != nil {
		writeRefusal(client, "3D000",
			fmt.Sprintf("pgbranch: cannot route to branch %q: %v", branch, err))
		return
	}
	startup.Parameters["database"] = dbname
	raw, err := startup.Encode(nil)
	if err != nil {
		writeRefusal(client, "08P01", "pgbranch: "+err.Error())
		return
	}
	backend, err := net.DialTimeout("tcp", addr, p.DialTimeout)
	if err != nil {
		writeRefusal(client, "08006", // connection_failure
			fmt.Sprintf("pgbranch: branch %q backend unreachable: %v", branch, err))
		return
	}
	defer backend.Close()
	if _, err := backend.Write(raw); err != nil {
		return
	}
	relay(client, backend)
}

// relay copies bytes in both directions until both sides are done. Each
// direction propagates EOF with a half-close (CloseWrite) so in-flight data
// in the other direction can still drain.
func relay(client, backend net.Conn) error {
	g := new(errgroup.Group)
	g.Go(func() error { return halfCopy(backend, client) })
	g.Go(func() error { return halfCopy(client, backend) })
	return g.Wait()
}

func halfCopy(dst, src net.Conn) error {
	_, err := io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
	return err
}
