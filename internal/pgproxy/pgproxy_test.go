package pgproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

type fakeResolver map[string]int

func (f fakeResolver) ResolveBranch(name string) (int, error) {
	port, ok := f[name]
	if !ok {
		return 0, registry.ErrNotFound
	}
	return port, nil
}

// startProxy runs a Proxy on an ephemeral listener and returns its address.
func startProxy(t *testing.T, r BranchResolver) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go New(r).Serve(ctx, lis)
	return lis.Addr().String()
}

func dialProxy(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { conn.Close() })
	return conn
}

// sendStartup crafts a StartupMessage with pgproto3's Frontend codec.
func sendStartup(t *testing.T, fe *pgproto3.Frontend, params map[string]string) {
	t.Helper()
	fe.Send(&pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber, Parameters: params})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
}

func recvError(t *testing.T, fe *pgproto3.Frontend) *pgproto3.ErrorResponse {
	t.Helper()
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	er, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("got %T, want *ErrorResponse", msg)
	}
	return er
}

// fakeBackend accepts one connection, asserts nothing itself, and hands the
// pgproto3 Backend plus startup message to fn.
func fakeBackend(t *testing.T, fn func(conn net.Conn, be *pgproto3.Backend, sm *pgproto3.StartupMessage)) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		be := pgproto3.NewBackend(conn, conn)
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			t.Errorf("backend startup: %v", err)
			return
		}
		sm, ok := msg.(*pgproto3.StartupMessage)
		if !ok {
			t.Errorf("backend got %T, want *StartupMessage", msg)
			return
		}
		fn(conn, be, sm)
	}()
	return lis.Addr().(*net.TCPAddr).Port
}

func TestSplitDatabase(t *testing.T) {
	tests := []struct {
		in, db, branch string
		ok             bool
	}{
		{"postgres@pr-1", "postgres", "pr-1", true},
		{"app@db@pr-1", "app@db", "pr-1", true}, // split on LAST @
		{"postgres@", "postgres", "", true},
		{"@pr-1", "", "pr-1", true},
		{"postgres", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		db, branch, ok := splitDatabase(tt.in)
		if db != tt.db || branch != tt.branch || ok != tt.ok {
			t.Errorf("splitDatabase(%q) = (%q,%q,%v), want (%q,%q,%v)",
				tt.in, db, branch, ok, tt.db, tt.branch, tt.ok)
		}
	}
}

func TestRoutesRewritesAndRelays(t *testing.T) {
	startupCh := make(chan *pgproto3.StartupMessage, 1)
	queryCh := make(chan string, 1)
	port := fakeBackend(t, func(conn net.Conn, be *pgproto3.Backend, sm *pgproto3.StartupMessage) {
		startupCh <- sm
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := be.Flush(); err != nil {
			return
		}
		msg, err := be.Receive()
		if err != nil {
			t.Errorf("backend receive: %v", err)
			return
		}
		q, ok := msg.(*pgproto3.Query)
		if !ok {
			t.Errorf("backend got %T, want *Query", msg)
			return
		}
		queryCh <- q.String
	})

	addr := startProxy(t, fakeResolver{"pr-1": port})
	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	// database with embedded @: only the LAST @ is the branch separator
	sendStartup(t, fe, map[string]string{"user": "alice", "database": "app@db@pr-1", "application_name": "unit"})

	sm := <-startupCh
	if got := sm.Parameters["database"]; got != "app@db" {
		t.Errorf("backend database = %q, want %q", got, "app@db")
	}
	if got := sm.Parameters["user"]; got != "alice" {
		t.Errorf("backend user = %q, want %q", got, "alice")
	}
	if got := sm.Parameters["application_name"]; got != "unit" {
		t.Errorf("backend application_name = %q, want %q", got, "unit")
	}

	// backend -> client relay
	if msg, err := fe.Receive(); err != nil {
		t.Fatal(err)
	} else if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("got %T, want *AuthenticationOk", msg)
	}
	if msg, err := fe.Receive(); err != nil {
		t.Fatal(err)
	} else if _, ok := msg.(*pgproto3.ReadyForQuery); !ok {
		t.Fatalf("got %T, want *ReadyForQuery", msg)
	}

	// client -> backend relay after startup
	fe.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := fe.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case q := <-queryCh:
		if q != "SELECT 1" {
			t.Errorf("backend query = %q", q)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backend never saw relayed query")
	}

	// backend close propagates to client as EOF
	if _, err := fe.Receive(); err == nil {
		t.Fatal("expected EOF after backend close")
	}
}

func TestSSLRequestAnsweredWithN(t *testing.T) {
	port := fakeBackend(t, func(conn net.Conn, be *pgproto3.Backend, sm *pgproto3.StartupMessage) {
		be.Send(&pgproto3.AuthenticationOk{})
		be.Flush()
	})
	addr := startProxy(t, fakeResolver{"pr-1": port})
	conn := dialProxy(t, addr)

	// hand-rolled SSLRequest: length 8, magic 80877103
	ssl := make([]byte, 8)
	binary.BigEndian.PutUint32(ssl[:4], 8)
	binary.BigEndian.PutUint32(ssl[4:], 80877103)
	if _, err := conn.Write(ssl); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 1)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != 'N' {
		t.Fatalf("SSLRequest answered %q, want 'N'", resp[0])
	}

	// client proceeds plaintext with a normal startup on the same conn
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres@pr-1"})
	if msg, err := fe.Receive(); err != nil {
		t.Fatal(err)
	} else if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("got %T, want *AuthenticationOk", msg)
	}
}

func TestCancelRequestClosedSilently(t *testing.T) {
	addr := startProxy(t, fakeResolver{})
	conn := dialProxy(t, addr)

	// hand-rolled CancelRequest: length 16, magic 80877102, pid, secret key
	cancel := make([]byte, 16)
	binary.BigEndian.PutUint32(cancel[:4], 16)
	binary.BigEndian.PutUint32(cancel[4:8], 80877102)
	binary.BigEndian.PutUint32(cancel[8:12], 1234)
	binary.BigEndian.PutUint32(cancel[12:16], 5678)
	if _, err := conn.Write(cancel); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("after CancelRequest read = (%d, %v), want (0, EOF)", n, err)
	}
}

func TestMissingBranchSuffixRefused(t *testing.T) {
	addr := startProxy(t, fakeResolver{"pr-1": 1})
	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres"})

	er := recvError(t, fe)
	if er.Code != "3D000" {
		t.Errorf("code = %q, want 3D000", er.Code)
	}
	if er.Severity != "FATAL" {
		t.Errorf("severity = %q, want FATAL", er.Severity)
	}
	if !strings.Contains(er.Message, "pgbranch: connect with dbname@branch") {
		t.Errorf("message %q missing guidance", er.Message)
	}
	if _, err := fe.Receive(); err == nil {
		t.Error("connection should be closed after refusal")
	}
}

func TestNoDatabaseParamRefused(t *testing.T) {
	addr := startProxy(t, fakeResolver{})
	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres"}) // database defaults to user; no @ either way

	er := recvError(t, fe)
	if er.Code != "3D000" {
		t.Errorf("code = %q, want 3D000", er.Code)
	}
	if !strings.Contains(er.Message, "pgbranch: connect with dbname@branch") {
		t.Errorf("message %q missing guidance", er.Message)
	}
}

func TestUnknownBranchRefused(t *testing.T) {
	addr := startProxy(t, fakeResolver{"pr-1": 1})
	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres@nope"})

	er := recvError(t, fe)
	if er.Code != "3D000" {
		t.Errorf("code = %q, want 3D000", er.Code)
	}
	if !strings.Contains(er.Message, `"nope"`) {
		t.Errorf("message %q does not name the branch", er.Message)
	}
}

func TestBackendDialFailureRefused(t *testing.T) {
	// resolve to a port nothing listens on
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := lis.Addr().(*net.TCPAddr).Port
	lis.Close()

	addr := startProxy(t, fakeResolver{"pr-1": deadPort})
	conn := dialProxy(t, addr)
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres@pr-1"})

	er := recvError(t, fe)
	if er.Code != "08006" {
		t.Errorf("code = %q, want 08006", er.Code)
	}
	if !strings.Contains(er.Message, "pgbranch") {
		t.Errorf("message %q missing pgbranch prefix", er.Message)
	}
}

func TestRegistryResolver(t *testing.T) {
	reg, err := registry.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	src := &registry.Source{Name: "main", ConnHost: "h", ConnPort: 5432, ConnUser: "u"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	ready := &registry.Branch{Name: "pr-1", SourceID: src.ID, RWVolume: "v1"}
	if err := reg.CreateBranch(ready); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkBranchReady(ready.ID, "cid", 54321); err != nil {
		t.Fatal(err)
	}
	creating := &registry.Branch{Name: "pr-2", SourceID: src.ID, RWVolume: "v2"}
	if err := reg.CreateBranch(creating); err != nil {
		t.Fatal(err)
	}

	r := &RegistryResolver{Reg: reg}
	port, err := r.ResolveBranch("pr-1")
	if err != nil || port != 54321 {
		t.Errorf("ResolveBranch(pr-1) = (%d, %v), want (54321, nil)", port, err)
	}
	if _, err := r.ResolveBranch("pr-2"); err == nil || !strings.Contains(err.Error(), "creating") {
		t.Errorf("ResolveBranch(pr-2) err = %v, want not-ready error", err)
	}
	if _, err := r.ResolveBranch("missing"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("ResolveBranch(missing) err = %v, want ErrNotFound", err)
	}
}
