package pgproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// A connection that sends nothing is dropped at FirstByteTimeout, well before
// the full StartupTimeout, so idle sockets cannot hold slots for long.
func TestFirstByteTimeoutDropsSilentClientEarly(t *testing.T) {
	p := New(fakeResolver{})
	p.FirstByteTimeout = 100 * time.Millisecond
	p.StartupTimeout = 5 * time.Second
	addr := startProxyWith(t, p)

	conn := dialProxy(t, addr)
	expectClosedWithin(t, conn, time.Second, "silent client")
}

// The first-byte deadline only covers the first byte: a client that starts
// promptly still gets the whole StartupTimeout for the rest of its packets.
func TestFirstByteTimeoutDoesNotCutSlowStartup(t *testing.T) {
	p := New(fakeResolver{})
	p.FirstByteTimeout = 100 * time.Millisecond
	p.StartupTimeout = 5 * time.Second
	addr := startProxyWith(t, p)

	conn := dialProxy(t, addr)
	ssl := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), sslRequestCode)
	if _, err := conn.Write(ssl[:2]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // past FirstByteTimeout, within StartupTimeout
	if _, err := conn.Write(ssl[2:]); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || b[0] != 'N' {
		t.Fatalf("SSLRequest answer = (%q, %v), want 'N'", b[0], err)
	}
}

// MaxStartupsPerIP caps one address's connections that have not finished
// startup. Authenticated sessions do not count against it, and a slot is
// released when its connection ends.
func TestPerIPStartupCap(t *testing.T) {
	srv := startPGServer(t, keyedSession(1, []byte{1, 2, 3, 4}))
	p := New(fakeResolver{"pr-1": srv.addr})
	p.MaxStartupsPerIP = 1
	p.FirstByteTimeout = 5 * time.Second
	addr := startProxyWith(t, p)

	// An authenticated session gives its startup slot back at ReadyForQuery.
	openSession(t, dialProxy(t, addr), "pr-1")

	// A connection still in startup holds the IP's only slot...
	pending := dialProxy(t, addr)
	waitFor(t, 2*time.Second, "pending connection counted", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.startups["127.0.0.1"] == 1
	})
	// ...so the next one from the same IP is refused fast.
	expectClosedWithin(t, dialProxy(t, addr), time.Second, "connection over the per-IP startup cap")

	pending.Close()
	waitFor(t, 3*time.Second, "slot released after the pending connection closed", func() bool { return slotFree(addr) })
}

// The authentication phase is bounded by AuthTimeout, not IdleTimeout: a
// client that stalls after the backend's auth challenge is dropped, and both
// its connection slot and the backend connection are released.
func TestAuthTimeoutBoundsAuthentication(t *testing.T) {
	backendDone := make(chan struct{})
	srv := startPGServer(t, func(conn net.Conn, be *pgproto3.Backend) {
		defer close(backendDone)
		be.Send(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}})
		be.Flush()
		io.Copy(io.Discard, conn) // wait for a SASL response that never comes
	})
	p := New(fakeResolver{"pr-1": srv.addr})
	p.MaxConns = 1
	p.AuthTimeout = 200 * time.Millisecond
	p.IdleTimeout = time.Hour
	addr := startProxyWith(t, p)

	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres@pr-1"})
	if msg, err := fe.Receive(); err != nil {
		t.Fatal(err)
	} else if _, ok := msg.(*pgproto3.AuthenticationSASL); !ok {
		t.Fatalf("got %T, want *AuthenticationSASL", msg)
	}
	expectClosedWithin(t, conn, 2*time.Second, "client stalled in authentication")
	select {
	case <-backendDone:
	case <-time.After(3 * time.Second):
		t.Fatal("backend connection still open after the auth timeout")
	}
	waitFor(t, 3*time.Second, "connection slot released", func() bool { return slotFree(addr) })
}

// When the backend closes (here right after the startup message, as a branch
// does at authentication_timeout), a client that stays silent and keeps its
// socket open must not pin the connection slot until IdleTimeout: the client
// sees EOF at once and the slot is freed within the close grace.
func TestBackendCloseFreesSlotPromptly(t *testing.T) {
	srv := startPGServer(t, func(conn net.Conn, be *pgproto3.Backend) {})
	p := New(fakeResolver{"pr-1": srv.addr})
	p.MaxConns = 1
	p.AuthTimeout = time.Hour
	p.IdleTimeout = time.Hour
	p.closeGrace = 200 * time.Millisecond
	addr := startProxyWith(t, p)

	conn := dialProxy(t, addr)
	sendStartup(t, pgproto3.NewFrontend(conn, conn), map[string]string{"user": "postgres", "database": "postgres@pr-1"})
	var b [1]byte
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(b[:]); err != io.EOF {
		t.Fatalf("client read after backend close = %v, want EOF", err)
	}
	// Keep conn open and silent: the proxy must still let the slot go.
	start := time.Now()
	waitFor(t, 3*time.Second, "connection slot released", func() bool { return slotFree(addr) })
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("slot released after %v, want within the close grace", elapsed)
	}
}

// A peer that stops reading cannot pin a session: the relay's blocked write
// times out after IdleTimeout, and the whole session (both connections and
// the slot) is torn down.
func TestNonReadingClientDoesNotPinSession(t *testing.T) {
	backendDone := make(chan error, 1)
	srv := startPGServer(t, func(conn net.Conn, be *pgproto3.Backend) {
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := be.Flush(); err != nil {
			backendDone <- err
			return
		}
		chunk := make([]byte, 64<<10)
		for {
			if _, err := conn.Write(chunk); err != nil {
				backendDone <- err // the proxy closed the backend connection
				return
			}
		}
	})
	p := New(fakeResolver{"pr-1": srv.addr})
	p.MaxConns = 1
	p.IdleTimeout = 300 * time.Millisecond
	p.closeGrace = 200 * time.Millisecond
	addr := startProxyWith(t, p)

	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres@pr-1"})
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := msg.(*pgproto3.ReadyForQuery); ok {
			break
		}
	}
	// Stop reading, keep the socket open.
	select {
	case err := <-backendDone:
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("fake backend hit its own deadline: the proxy never closed the session")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("session with a non-reading client was never torn down")
	}
	waitFor(t, 3*time.Second, "connection slot released", func() bool { return slotFree(addr) })
}

// flakyListener returns the queued errors from Accept before delegating.
type flakyListener struct {
	net.Listener
	errs chan error
}

func (l *flakyListener) Accept() (net.Conn, error) {
	select {
	case err := <-l.errs:
		return nil, err
	default:
		return l.Listener.Accept()
	}
}

func newFlakyListener(t *testing.T, errs ...error) *flakyListener {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	l := &flakyListener{Listener: lis, errs: make(chan error, len(errs))}
	for _, e := range errs {
		l.errs <- e
	}
	return l
}

func acceptErr(errno syscall.Errno) error {
	return &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", errno)}
}

// Running out of file descriptors (or buffers) is transient: Serve backs off
// and keeps accepting instead of returning, which would stop branchd.
func TestServeRetriesTemporaryAcceptErrors(t *testing.T) {
	lis := newFlakyListener(t, acceptErr(syscall.EMFILE), acceptErr(syscall.ENFILE), acceptErr(syscall.ENOBUFS))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- New(fakeResolver{}).Serve(ctx, lis) }()

	if !slotFree(lis.Addr().String()) {
		select {
		case err := <-served:
			t.Fatalf("Serve returned %v on a temporary accept error", err)
		default:
			t.Fatal("proxy did not serve a connection after temporary accept errors")
		}
	}
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after ctx cancel")
	}
}

// A non-temporary Accept error means the listener is broken: Serve returns it.
func TestServeReturnsPermanentAcceptError(t *testing.T) {
	boom := errors.New("listener broken")
	lis := newFlakyListener(t, boom)
	err := New(fakeResolver{}).Serve(context.Background(), lis)
	if !errors.Is(err, boom) {
		t.Fatalf("Serve = %v, want %v", err, boom)
	}
}

// Cancelling ctx while Serve is backing off returns promptly.
func TestServeStopsDuringAcceptBackoff(t *testing.T) {
	errs := make([]error, 50)
	for i := range errs {
		errs[i] = acceptErr(syscall.EMFILE)
	}
	lis := newFlakyListener(t, errs...)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- New(fakeResolver{}).Serve(ctx, lis) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after ctx cancel during backoff")
	}
}

func TestIsTemporaryAcceptError(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{acceptErr(syscall.EMFILE), true},
		{acceptErr(syscall.ENFILE), true},
		{acceptErr(syscall.ENOBUFS), true},
		{acceptErr(syscall.ENOMEM), true},
		{&net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed}, false},
		{errors.New("listener broken"), false},
	} {
		if got := isTemporaryAcceptError(tt.err); got != tt.want {
			t.Errorf("isTemporaryAcceptError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// A Proxy built as a bare struct literal gets the same protections as New():
// every zero field falls back to its default. Only a negative cap disables it.
func TestZeroValueProxyUsesDefaults(t *testing.T) {
	var p Proxy
	for _, tt := range []struct {
		name      string
		got, want time.Duration
	}{
		{"DialTimeout", p.dialTimeout(), defaultDialTimeout},
		{"FirstByteTimeout", p.firstByteTimeout(), defaultFirstByteTimeout},
		{"StartupTimeout", p.startupTimeout(), defaultStartupTimeout},
		{"AuthTimeout", p.authTimeout(), defaultAuthTimeout},
		{"IdleTimeout", p.idleTimeout(), defaultIdleTimeout},
	} {
		if tt.got != tt.want {
			t.Errorf("zero %s -> %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if got := p.maxConns(); got != defaultMaxConns {
		t.Errorf("zero MaxConns -> %d, want %d", got, defaultMaxConns)
	}
	if got := p.maxStartupsPerIP(); got != defaultMaxStartupsPerIP {
		t.Errorf("zero MaxStartupsPerIP -> %d, want %d", got, defaultMaxStartupsPerIP)
	}
	p.MaxConns, p.MaxStartupsPerIP = -1, -1
	if p.maxConns() != 0 || p.maxStartupsPerIP() != 0 {
		t.Errorf("negative caps -> (%d, %d), want (0, 0) = disabled", p.maxConns(), p.maxStartupsPerIP())
	}

	// And it routes like one built by New.
	srv := startPGServer(t, keyedSession(1, []byte{1, 2, 3, 4}))
	addr := startProxyWith(t, &Proxy{Resolver: fakeResolver{"pr-1": srv.addr}})
	openSession(t, dialProxy(t, addr), "pr-1")
}
