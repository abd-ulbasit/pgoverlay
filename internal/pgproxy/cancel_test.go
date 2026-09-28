package pgproxy

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

func cancelFrame(pid uint32, key []byte) []byte {
	frame, err := (&pgproto3.CancelRequest{ProcessID: pid, SecretKey: key}).Encode(nil)
	if err != nil {
		panic(err)
	}
	return frame
}

// sendCancel writes frame on conn and waits for the proxy to close it, the
// way libpq and pgx do.
func sendCancel(t *testing.T, conn net.Conn, frame []byte) {
	t.Helper()
	if _, err := conn.Write(frame); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if n, err := conn.Read(buf); n != 0 || err == nil {
		t.Fatalf("after CancelRequest read = (%d, %v), want (0, closed)", n, err)
	}
}

func expectCancel(t *testing.T, srv *pgServer, want []byte) {
	t.Helper()
	// The proxy closes the client's cancel connection only after the backend
	// has closed its own, so the frame must already be here: no waiting.
	select {
	case got := <-srv.cancels:
		if !bytes.Equal(got, want) {
			t.Fatalf("backend got cancel frame %x, want %x", got, want)
		}
	default:
		t.Fatal("cancel request was not forwarded to the session's backend (or the client was released before it was)")
	}
}

func expectNoCancel(t *testing.T, srvs ...*pgServer) {
	t.Helper()
	time.Sleep(300 * time.Millisecond) // a forwarded cancel would have arrived by now
	for _, srv := range srvs {
		select {
		case got := <-srv.cancels:
			t.Fatalf("cancel frame %x was forwarded to %s, want it dropped", got, srv.addr)
		default:
		}
	}
}

// A CancelRequest carrying a live session's key reaches that session's backend
// unchanged, while other branches' backends see nothing.
func TestCancelRequestForwardedToSessionBackend(t *testing.T) {
	key := []byte{0xde, 0xad, 0xbe, 0xef}
	srv1 := startPGServer(t, keyedSession(4242, key))
	srv2 := startPGServer(t, keyedSession(7, []byte{1, 2, 3, 4}))
	addr := startProxy(t, fakeResolver{"pr-1": srv1.addr, "pr-2": srv2.addr})

	got := openSession(t, dialProxy(t, addr), "pr-1")
	if got.ProcessID != 4242 || !bytes.Equal(got.SecretKey, key) {
		t.Fatalf("client saw BackendKeyData(%d, %x), want (4242, %x)", got.ProcessID, got.SecretKey, key)
	}
	openSession(t, dialProxy(t, addr), "pr-2")

	frame := cancelFrame(4242, key)
	sendCancel(t, dialProxy(t, addr), frame)
	expectCancel(t, srv1, frame)
	expectNoCancel(t, srv2)
}

// Unknown keys are dropped silently: the client's connection is closed and no
// backend is contacted.
func TestCancelRequestUnknownKeyDropped(t *testing.T) {
	srv := startPGServer(t, keyedSession(4242, []byte{0xde, 0xad, 0xbe, 0xef}))
	addr := startProxy(t, fakeResolver{"pr-1": srv.addr})
	openSession(t, dialProxy(t, addr), "pr-1")

	sendCancel(t, dialProxy(t, addr), cancelFrame(4242, []byte{0, 0, 0, 1})) // right pid, wrong key
	sendCancel(t, dialProxy(t, addr), cancelFrame(4243, []byte{0xde, 0xad, 0xbe, 0xef}))
	expectNoCancel(t, srv)
}

// A session's key is forgotten once the session ends, so a late cancel cannot
// reach whatever the backend runs next under that pid.
func TestCancelKeyRemovedWhenSessionEnds(t *testing.T) {
	key := []byte{0xde, 0xad, 0xbe, 0xef}
	srv := startPGServer(t, keyedSession(4242, key))
	p := New(fakeResolver{"pr-1": srv.addr})
	addr := startProxyWith(t, p)

	conn := dialProxy(t, addr)
	openSession(t, conn, "pr-1")
	if n := p.cancels.len(); n != 1 {
		t.Fatalf("live cancel keys = %d, want 1", n)
	}
	conn.Close()
	waitFor(t, 3*time.Second, "cancel key removed after the session ended", func() bool { return p.cancels.len() == 0 })

	sendCancel(t, dialProxy(t, addr), cancelFrame(4242, key))
	expectNoCancel(t, srv)
}

// Cancellation works when the client uses TLS for the session, for the cancel
// connection (libpq 17+ and pgx encrypt it the same way as the session), or
// for neither.
func TestCancelRequestOverTLS(t *testing.T) {
	key := []byte{9, 8, 7, 6}
	srv := startPGServer(t, keyedSession(99, key))
	p := New(fakeResolver{"pr-1": srv.addr})
	p.TLSConfig = testTLSConfig(t)
	addr := startProxyWith(t, p)

	dialTLS := func() net.Conn {
		conn := dialProxy(t, addr)
		if got := sendSSLRequest(t, conn); got != 'S' {
			t.Fatalf("SSLRequest answered %q, want 'S'", got)
		}
		tconn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
		if err := tconn.Handshake(); err != nil {
			t.Fatalf("TLS handshake: %v", err)
		}
		return tconn
	}

	openSession(t, dialTLS(), "pr-1")
	frame := cancelFrame(99, key)

	sendCancel(t, dialTLS(), frame)
	expectCancel(t, srv, frame)

	sendCancel(t, dialProxy(t, addr), frame) // plaintext cancel for a TLS session
	expectCancel(t, srv, frame)
}

// With protocol 3.2 the secret key is variable-length (up to 256 bytes) and
// the CancelRequest grows with it; the proxy must forward the whole key.
func TestCancelRequestLongSecretKey(t *testing.T) {
	key := bytes.Repeat([]byte{0xab, 0xcd}, 16) // 32 bytes, what PostgreSQL 18 sends
	srv := startPGServer(t, keyedSession(31337, key))
	addr := startProxy(t, fakeResolver{"pr-1": srv.addr})

	got := openSession(t, dialProxy(t, addr), "pr-1")
	if !bytes.Equal(got.SecretKey, key) {
		t.Fatalf("client saw secret key %x, want %x", got.SecretKey, key)
	}
	frame := cancelFrame(31337, key)
	sendCancel(t, dialProxy(t, addr), frame)
	expectCancel(t, srv, frame)

	sendCancel(t, dialProxy(t, addr), cancelFrame(31337, key[:4])) // truncated key: unknown
	expectNoCancel(t, srv)
}

// Two branches (separate Postgres servers) can hand out the same key. The
// proxy cannot tell which session the cancel is for, so it forwards to
// neither rather than risk cancelling the wrong client's query.
func TestCancelRequestAmbiguousKeyDropped(t *testing.T) {
	key := []byte{1, 1, 1, 1}
	srv1 := startPGServer(t, keyedSession(100, key))
	srv2 := startPGServer(t, keyedSession(100, key))
	p := New(fakeResolver{"pr-1": srv1.addr, "pr-2": srv2.addr})
	addr := startProxyWith(t, p)

	openSession(t, dialProxy(t, addr), "pr-1")
	conn2 := dialProxy(t, addr)
	openSession(t, conn2, "pr-2")

	frame := cancelFrame(100, key)
	sendCancel(t, dialProxy(t, addr), frame)
	expectNoCancel(t, srv1, srv2)

	// Once one of them ends the key is unambiguous again.
	conn2.Close()
	waitFor(t, 3*time.Second, "pr-2 session's key removed", func() bool {
		_, ok := p.cancels.lookup(string(frame[8:]))
		return ok
	})
	sendCancel(t, dialProxy(t, addr), frame)
	expectCancel(t, srv1, frame)
}

func TestCancelMap(t *testing.T) {
	var m cancelMap
	if _, ok := m.lookup("k"); ok {
		t.Fatal("lookup on empty map succeeded")
	}
	m.add("k", "a:1")
	if addr, ok := m.lookup("k"); !ok || addr != "a:1" {
		t.Fatalf("lookup = (%q, %v), want (a:1, true)", addr, ok)
	}
	m.add("k", "b:2")
	if _, ok := m.lookup("k"); ok {
		t.Fatal("lookup of a key held by two sessions succeeded, want ambiguous")
	}
	m.remove("k", "a:1")
	if addr, ok := m.lookup("k"); !ok || addr != "b:2" {
		t.Fatalf("lookup after removing a:1 = (%q, %v), want (b:2, true)", addr, ok)
	}
	m.remove("k", "b:2")
	m.remove("k", "b:2") // idempotent
	if n := m.len(); n != 0 {
		t.Fatalf("len = %d, want 0", n)
	}
}
