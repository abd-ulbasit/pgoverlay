package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/pgctl"
	"github.com/abd-ulbasit/pgbranch/internal/pgproxy"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

const itToken = "it-token"

func itDo(t *testing.T, ts *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+itToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

func queryInt(t *testing.T, ctx context.Context, conn, q string) int {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatalf("connect %s: %v", conn, err)
	}
	defer c.Close(ctx)
	var n int
	if err := c.QueryRow(ctx, q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func exec(t *testing.T, ctx context.Context, conn, q string) {
	t.Helper()
	c, err := pgx.Connect(ctx, conn)
	if err != nil {
		t.Fatalf("connect %s: %v", conn, err)
	}
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestPhase2DataPlane drives branchd's components in-process on ephemeral
// ports against a real docker driver: REST source seed, TTL branch reaped by
// the reaper, proxy routing via dbname@branch, and REST reset re-cloning a
// branch on a fresh container.
func TestPhase2DataPlane(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	host, port, network, hostConn := pgctl.StartSourcePG(t, ctx)
	exec(t, ctx, hostConn, `CREATE TABLE accounts(id int primary key, balance int);
		INSERT INTO accounts SELECT i, 100 FROM generate_series(1,1000) i`)

	drv, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(t.TempDir() + "/it.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	eng := engine.New(reg, drv, "postgres:17")

	ts := httptest.NewServer(api.New(eng, reg, itToken, nil, nil, 0).Handler())
	t.Cleanup(ts.Close)

	// best-effort cleanup of managed resources if assertions below bail out
	// (no t.Fatal here: 404s and errors are fine on the happy path)
	t.Cleanup(func() {
		for _, path := range []string{"/v1/branches/ephemeral", "/v1/branches/keep", "/v1/sources/api-main"} {
			req, _ := http.NewRequest("DELETE", ts.URL+path, nil)
			req.Header.Set("Authorization", "Bearer "+itToken)
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bgCtx, stopBg := context.WithCancel(context.Background())
	t.Cleanup(stopBg)
	go pgproxy.New(&pgproxy.RegistryResolver{Reg: reg}).Serve(bgCtx, lis)
	proxyAddr := lis.Addr().String()

	// 1. REST: create + seed the source (password travels in the body)
	code, body := itDo(t, ts, "POST", "/v1/sources", api.CreateSourceRequest{
		Name: "api-main", Host: host, Port: port, User: "postgres",
		Database: "postgres", Network: network, PGVersion: "17", Password: "secret",
	})
	if code != http.StatusCreated {
		t.Fatalf("create source: code=%d body=%s", code, body)
	}

	// 2. REST: branch with a 2s TTL
	code, body = itDo(t, ts, "POST", "/v1/branches", api.CreateBranchRequest{
		Name: "ephemeral", Source: "api-main", TTLSeconds: 2,
	})
	if code != http.StatusCreated {
		t.Fatalf("create ephemeral: code=%d body=%s", code, body)
	}
	var eph api.Branch
	json.Unmarshal(body, &eph)
	if eph.Port == 0 || eph.ProxyDatabase != "postgres@ephemeral" {
		t.Fatalf("ephemeral=%+v", eph)
	}

	// 3. through the proxy with dbname@branch (SSLRequest 'N' + SCRAM relay)
	proxyConn := fmt.Sprintf("postgres://postgres:secret@%s/postgres@ephemeral", proxyAddr)
	if n := queryInt(t, ctx, proxyConn, `SELECT count(*) FROM accounts`); n != 1000 {
		t.Fatalf("rows through proxy = %d", n)
	}

	// 4. reaper destroys the expired branch. Started after the connectivity
	// check: provisioning takes longer than the 2s TTL, so a reaper running
	// from t=0 could legitimately destroy the branch before step 3.
	go eng.RunReconcile(bgCtx, time.Second, 10*time.Minute, nil)
	deadline := time.Now().Add(45 * time.Second)
	for {
		code, body = itDo(t, ts, "GET", "/v1/branches", nil)
		if code != http.StatusOK {
			t.Fatalf("list branches: code=%d body=%s", code, body)
		}
		var branches []api.Branch
		json.Unmarshal(body, &branches)
		gone := true
		for _, b := range branches {
			if b.Name == "ephemeral" {
				gone = false
			}
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reaper never destroyed ephemeral; branches=%s", body)
		}
		time.Sleep(time.Second)
	}

	// 5. reset flow on a no-TTL branch: writes vanish, container is new
	code, body = itDo(t, ts, "POST", "/v1/branches", api.CreateBranchRequest{Name: "keep", Source: "api-main"})
	if code != http.StatusCreated {
		t.Fatalf("create keep: code=%d body=%s", code, body)
	}
	keepConn := fmt.Sprintf("postgres://postgres:secret@%s/postgres@keep", proxyAddr)
	exec(t, ctx, keepConn, `UPDATE accounts SET balance = 0 WHERE id = 1`)
	if n := queryInt(t, ctx, keepConn, `SELECT balance FROM accounts WHERE id = 1`); n != 0 {
		t.Fatalf("write not visible before reset: balance=%d", n)
	}
	before, err := reg.GetBranchByName("keep")
	if err != nil {
		t.Fatal(err)
	}

	code, body = itDo(t, ts, "POST", "/v1/branches/keep/reset", nil)
	if code != http.StatusOK {
		t.Fatalf("reset keep: code=%d body=%s", code, body)
	}
	var reset api.Branch
	json.Unmarshal(body, &reset)
	if reset.State != "ready" || reset.Port == 0 {
		t.Fatalf("after reset: %+v", reset)
	}
	after, err := reg.GetBranchByName("keep")
	if err != nil {
		t.Fatal(err)
	}
	if after.ContainerID == before.ContainerID {
		t.Fatalf("container not recreated on reset: %s", after.ContainerID)
	}
	// data re-cloned from the source snapshot: the write is gone
	if n := queryInt(t, ctx, keepConn, `SELECT balance FROM accounts WHERE id = 1`); n != 100 {
		t.Fatalf("reset did not discard writes: balance=%d", n)
	}
	if n := queryInt(t, ctx, keepConn, `SELECT count(*) FROM accounts`); n != 1000 {
		t.Fatalf("rows after reset = %d", n)
	}

	// 6. tear down via REST
	if code, body = itDo(t, ts, "DELETE", "/v1/branches/keep", nil); code != http.StatusNoContent {
		t.Fatalf("destroy keep: code=%d body=%s", code, body)
	}
	if code, body = itDo(t, ts, "DELETE", "/v1/sources/api-main", nil); code != http.StatusNoContent {
		t.Fatalf("remove source: code=%d body=%s", code, body)
	}
}
