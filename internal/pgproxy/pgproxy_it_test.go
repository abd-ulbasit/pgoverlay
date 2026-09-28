package pgproxy_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/pgproxy"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// selfSignedTLSConfig builds a server tls.Config for 127.0.0.1/localhost.
func selfSignedTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pgoverlay-it"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// TestProxyIntegration routes a real pgx connection through the proxy to a
// real branch: database=postgres@proxy-pr-1, SCRAM password auth relayed untouched.
func TestProxyIntegration(t *testing.T) {
	if os.Getenv("PGOVERLAY_IT") != "1" {
		t.Skip("set PGOVERLAY_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	{
		c, err := pgx.Connect(ctx, hostConn)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Exec(ctx, `CREATE TABLE accounts(id int primary key, balance int);
			INSERT INTO accounts SELECT i, 100 FROM generate_series(1,1000) i`); err != nil {
			t.Fatal(err)
		}
		c.Close(ctx)
	}

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the destroy cleanups below so it runs after them
	// (cleanups are LIFO).
	t.Cleanup(func() { reg.Close() })
	e := engine.New(reg, d, "postgres:17")

	src := &registry.Source{Name: "proxy-main", PGVersion: "17", ConnHost: host, ConnPort: port, ConnUser: "postgres", Network: network}
	if err := e.AddSource(ctx, src, "secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), src.Volume) })

	if _, err := e.CreateBranch(ctx, "proxy-pr-1", "proxy-main", 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.DestroyBranch(context.Background(), "proxy-pr-1"); err != nil {
			t.Errorf("destroy pr-1: %v", err)
		}
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	t.Cleanup(stopProxy)
	go pgproxy.New(&pgproxy.RegistryResolver{Reg: reg}).Serve(proxyCtx, lis)
	proxyAddr := lis.Addr().String()

	// happy path: pgx sends SSLRequest (sslmode=prefer) -> 'N' -> plaintext
	// startup with database=postgres@proxy-pr-1 -> SCRAM auth relayed -> query.
	conn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@proxy-pr-1", proxyAddr))
	if err != nil {
		t.Fatalf("connect through proxy: %v", err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("query through proxy: %v", err)
	}
	if n != 1000 {
		t.Fatalf("rows through proxy = %d, want 1000", n)
	}
	// write through the proxy stays on the branch
	if _, err := conn.Exec(ctx, `UPDATE accounts SET balance = 0 WHERE id = 1`); err != nil {
		t.Fatalf("write through proxy: %v", err)
	}
	conn.Close(ctx)

	// negative: no @ in the database name -> 3D000 with guidance
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres", proxyAddr))
	if err == nil {
		t.Fatal("connect without @branch suffix should fail")
	}
	if !strings.Contains(err.Error(), "pgoverlay: connect with dbname@branch") {
		t.Errorf("missing-suffix error %q lacks guidance", err)
	}

	// negative: unknown branch -> generic 3D000 that does NOT leak the branch
	// name or state (anti-enumeration; the real reason is logged server-side).
	_, err = pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@nope", proxyAddr))
	if err == nil {
		t.Fatal("connect to unknown branch should fail")
	}
	// The server message must be the generic refusal and must NOT distinguish
	// not-found from not-ready (the enumeration signal C2 removed). (The DSN
	// "postgres@nope" is echoed by pgx itself, not by our server message.)
	if !strings.Contains(err.Error(), "database not available") {
		t.Errorf("unknown-branch error %q lacks the generic refusal", err)
	}
	if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "not ready") {
		t.Errorf("unknown-branch error %q leaks branch existence/state (enumeration)", err)
	}

	// TLS: a second proxy with a self-signed cert; pgx sslmode=require sends
	// SSLRequest -> 'S' -> TLS handshake -> startup routes, SCRAM relayed.
	tlsLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsProxy := pgproxy.New(&pgproxy.RegistryResolver{Reg: reg})
	tlsProxy.TLSConfig = selfSignedTLSConfig(t)
	go tlsProxy.Serve(proxyCtx, tlsLis)
	tlsAddr := tlsLis.Addr().String()

	tlsConn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@proxy-pr-1?sslmode=require", tlsAddr))
	if err != nil {
		t.Fatalf("connect through TLS proxy (sslmode=require): %v", err)
	}
	if err := tlsConn.QueryRow(ctx, `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("query through TLS proxy: %v", err)
	}
	if n != 1000 {
		t.Fatalf("rows through TLS proxy = %d, want 1000", n)
	}
	tlsConn.Close(ctx)

	// sslmode=disable against the plaintext proxy is unchanged ('N' path was
	// exercised above); against the TLS proxy, plaintext startup still works
	// because TLS is opportunistic (client chooses via SSLRequest).
	plainConn, err := pgx.Connect(ctx, fmt.Sprintf("postgres://postgres:secret@%s/postgres@proxy-pr-1?sslmode=disable", tlsAddr))
	if err != nil {
		t.Fatalf("plaintext connect to TLS-enabled proxy: %v", err)
	}
	if err := plainConn.QueryRow(ctx, `SELECT 1`).Scan(&n); err != nil {
		t.Fatalf("plaintext query on TLS-enabled proxy: %v", err)
	}
	plainConn.Close(ctx)

	// Cancellation through the router (#18): a CancelRequest sent to the proxy
	// reaches the branch that runs the statement, whether the session and the
	// cancel connection are plaintext or TLS (pgx encrypts the cancel
	// connection like the session, as libpq 17+ does).
	assertCancelThroughProxy(t, ctx, "plaintext",
		fmt.Sprintf("postgres://postgres:secret@%s/postgres@proxy-pr-1?sslmode=disable", proxyAddr))
	assertCancelThroughProxy(t, ctx, "tls",
		fmt.Sprintf("postgres://postgres:secret@%s/postgres@proxy-pr-1?sslmode=require", tlsAddr))
}

// assertCancelThroughProxy runs a long statement through the proxy at url,
// sends a CancelRequest through the proxy once the statement is running on the
// branch, and requires the statement to fail with query_canceled promptly.
func assertCancelThroughProxy(t *testing.T, ctx context.Context, name, url string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("%s: connect through proxy: %v", name, err)
	}
	defer conn.Close(ctx)
	watcher, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("%s: connect watcher through proxy: %v", name, err)
	}
	defer watcher.Close(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, "SELECT pg_sleep(60)")
		done <- err
	}()
	// Wait until the statement runs on the branch, so the cancel has a target.
	pid := conn.PgConn().PID()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var active bool
		if err := watcher.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid = $1 AND state = 'active' AND query LIKE 'SELECT pg_sleep%')`, pid).Scan(&active); err != nil {
			t.Fatalf("%s: poll pg_stat_activity: %v", name, err)
		}
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: pg_sleep never showed up as active on the branch", name)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := conn.PgConn().CancelRequest(ctx); err != nil {
		t.Fatalf("%s: send cancel request through proxy: %v", name, err)
	}
	select {
	case err := <-done:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" {
			t.Fatalf("%s: statement ended with %v, want query_canceled (57014)", name, err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("%s: statement still running 15s after the cancel request: the router did not forward it", name)
	}
}
