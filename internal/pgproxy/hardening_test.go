package pgproxy

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

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
		{"StartupTimeout", p.startupTimeout(), defaultStartupTimeout},
		{"IdleTimeout", p.idleTimeout(), defaultIdleTimeout},
	} {
		if tt.got != tt.want {
			t.Errorf("zero %s -> %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if got := p.maxConns(); got != defaultMaxConns {
		t.Errorf("zero MaxConns -> %d, want %d", got, defaultMaxConns)
	}
	p.MaxConns = -1
	if p.maxConns() != 0 {
		t.Errorf("negative MaxConns -> %d, want 0 = disabled", p.maxConns())
	}

	// And it routes like one built by New.
	srv := startPGServer(t, keyedSession(1, []byte{1, 2, 3, 4}))
	addr := startProxyWith(t, &Proxy{Resolver: fakeResolver{"pr-1": srv.addr}})
	openSession(t, dialProxy(t, addr), "pr-1")
}
