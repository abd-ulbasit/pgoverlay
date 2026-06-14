package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/metrics"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// fakeDriver is a local copy of the engine tests' fake (test fakes are not
// exported across packages on purpose).
type fakeDriver struct {
	volumes    map[string]bool
	containers map[string]bool
	failStart  bool
	starts     int
	execs      [][]string // every Exec call, in order
	execOutErr error      // returned by every ExecOutput call when set
}

func newFake() *fakeDriver {
	return &fakeDriver{volumes: map[string]bool{}, containers: map[string]bool{}}
}
func (f *fakeDriver) EnsureImage(ctx context.Context, image string) error { return nil }
func (f *fakeDriver) CreateVolume(ctx context.Context, name string, l map[string]string) error {
	f.volumes[name] = true
	return nil
}
func (f *fakeDriver) RemoveVolume(ctx context.Context, name string) error {
	delete(f.volumes, name)
	return nil
}
func (f *fakeDriver) CloneVolume(ctx context.Context, src, dst string, l map[string]string) error {
	f.volumes[dst] = true
	return nil
}

// RunHelper returns canned du output so the usage endpoint has something to parse.
func (f *fakeDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) (string, error) {
	return "4096\t/pgbranch/rw", nil
}
func (f *fakeDriver) StartBranch(ctx context.Context, s runtime.BranchSpec) (string, error) {
	if f.failStart {
		return "", errors.New("boom")
	}
	f.starts++
	f.containers["cid-"+s.Name] = true
	return "cid-" + s.Name, nil
}
func (f *fakeDriver) Exec(ctx context.Context, id string, cmd []string) error {
	f.execs = append(f.execs, cmd)
	return nil
}

// ExecOutput returns canned in-container command output: schema dumps that
// differ between the throwaway base clone (its container name carries the
// diff- prefix) and the target branch, plus row-estimate lines, so the diff
// endpoint has something realistic to assemble.
func (f *fakeDriver) ExecOutput(ctx context.Context, id string, cmd []string) (string, error) {
	if f.execOutErr != nil {
		return "", f.execOutErr
	}
	isBase := strings.Contains(id, "pgbranch-br-diff-")
	joined := strings.Join(cmd, " ")
	if len(cmd) > 0 && cmd[0] == "pg_dump" {
		if isBase {
			return "CREATE TABLE users (id integer);\n", nil
		}
		return "CREATE TABLE users (id integer);\nCREATE TABLE added (x integer);\n", nil
	}
	switch {
	case strings.Contains(joined, "reltuples"):
		if isBase {
			return "users|100\n", nil
		}
		return "added|7\nusers|100\n", nil
	case strings.Contains(joined, "indisprimary"):
		if strings.Contains(joined, "'added'") {
			return "x\n", nil // added has PK x
		}
		return "", nil
	case strings.Contains(joined, "to_jsonb"):
		if isBase {
			return "", nil // base has no rows in the new table
		}
		return `{"x": 1}` + "\n" + `{"x": 2}` + "\n", nil
	}
	return "", nil
}
func (f *fakeDriver) Inspect(ctx context.Context, id string) (runtime.ContainerInfo, error) {
	return runtime.ContainerInfo{ID: id, Running: f.containers[id], Host: "127.0.0.1", Port: 54321}, nil
}
func (f *fakeDriver) StopRemove(ctx context.Context, id string) error {
	delete(f.containers, id)
	return nil
}
func (f *fakeDriver) ListManaged(ctx context.Context) ([]runtime.ContainerInfo, error) {
	var out []runtime.ContainerInfo
	for id := range f.containers {
		out = append(out, runtime.ContainerInfo{ID: id, Running: true})
	}
	return out, nil
}
func (f *fakeDriver) ListManagedVolumes(ctx context.Context, instanceID string) ([]string, error) {
	var out []string
	for name := range f.volumes {
		out = append(out, name)
	}
	return out, nil
}

const testToken = "sekrit"

func newTestServer(t *testing.T, opts ...engine.Option) (*httptest.Server, *fakeDriver) {
	t.Helper()
	d := newFake()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	eng := engine.New(reg, d, "postgres:17", opts...)
	m := metrics.New()
	m.SetStateCounter(reg)
	ready := func(ctx context.Context) error {
		if err := reg.Ping(ctx); err != nil {
			return err
		}
		_, err := d.ListManaged(ctx)
		return err
	}
	ts := httptest.NewServer(New(eng, reg, testToken, m.Handler(), ready, 0).Handler())
	t.Cleanup(ts.Close)
	return ts, d
}

// newTestServerWithLeader builds the API like newTestServer but returns the
// *Server too, so leader-gate tests can flip its gate.
func newTestServerWithLeader(t *testing.T, opts ...engine.Option) (*httptest.Server, *Server) {
	t.Helper()
	d := newFake()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	eng := engine.New(reg, d, "postgres:17", opts...)
	m := metrics.New()
	m.SetStateCounter(reg)
	ready := func(ctx context.Context) error {
		if err := reg.Ping(ctx); err != nil {
			return err
		}
		_, err := d.ListManaged(ctx)
		return err
	}
	srv := New(eng, reg, testToken, m.Handler(), ready, 0)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv
}

// do sends an authenticated JSON request; token "" sends no Authorization.
func do(t *testing.T, ts *httptest.Server, token, method, path string, body any) (int, []byte) {
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
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func mustUnmarshal[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("unmarshal %q: %v", data, err)
	}
	return v
}

func addSource(t *testing.T, ts *httptest.Server) Source {
	t.Helper()
	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Port: 5432, User: "postgres",
		Database: "postgres", PGVersion: "17", Password: "secret",
	})
	if code != http.StatusCreated {
		t.Fatalf("create source: code=%d body=%s", code, body)
	}
	return mustUnmarshal[Source](t, body)
}

func TestHealthzUnauthenticated(t *testing.T) {
	ts, _ := newTestServer(t)
	code, _ := do(t, ts, "", "GET", "/healthz", nil)
	if code != http.StatusOK {
		t.Fatalf("healthz code=%d", code)
	}
}

func TestMetricsUnauthenticated(t *testing.T) {
	ts, _ := newTestServer(t)
	// seed a source + branch so the collector reports the branches gauge
	// (GROUP BY emits a series only for states that have rows)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create branch: code=%d body=%s", code, body)
	}
	code, body := do(t, ts, "", "GET", "/metrics", nil)
	if code != http.StatusOK {
		t.Fatalf("metrics code=%d (want 200, no auth)", code)
	}
	if !strings.Contains(string(body), "pgbranch_branches_total") {
		t.Fatalf("metrics body missing pgbranch_branches_total:\n%s", body)
	}
}

func TestReadyzOpenAndClosed(t *testing.T) {
	d := newFake()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(reg, d, "postgres:17")
	ready := func(ctx context.Context) error {
		if err := reg.Ping(ctx); err != nil {
			return err
		}
		_, err := d.ListManaged(ctx)
		return err
	}
	ts := httptest.NewServer(New(eng, reg, testToken, metrics.New().Handler(), ready, 0).Handler())
	t.Cleanup(ts.Close)

	// registry open + driver responds -> ready
	if code, body := do(t, ts, "", "GET", "/readyz", nil); code != http.StatusOK {
		t.Fatalf("readyz (open) code=%d body=%s want 200 no-auth", code, body)
	}
	// close the registry: the trivial Ping query now fails -> 503
	reg.Close()
	if code, _ := do(t, ts, "", "GET", "/readyz", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz (closed registry) code=%d want 503", code)
	}
}

func TestAuthRejection(t *testing.T) {
	ts, _ := newTestServer(t)
	for name, token := range map[string]string{"missing": "", "wrong": "nope"} {
		code, body := do(t, ts, token, "GET", "/v1/branches", nil)
		if code != http.StatusUnauthorized {
			t.Fatalf("%s token: code=%d", name, code)
		}
		e := mustUnmarshal[map[string]string](t, body)
		if e["error"] == "" {
			t.Fatalf("%s token: no error message in %s", name, body)
		}
	}
	if code, _ := do(t, ts, testToken, "GET", "/v1/branches", nil); code != http.StatusOK {
		t.Fatalf("valid token rejected: code=%d", code)
	}
}

func TestSourceAndBranchLifecycle(t *testing.T) {
	ts, _ := newTestServer(t)

	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Port: 5432, User: "postgres",
		Database: "postgres", PGVersion: "17", Password: "secret",
	})
	if code != http.StatusCreated {
		t.Fatalf("create source: code=%d body=%s", code, body)
	}
	if strings.Contains(string(body), "secret") {
		t.Fatalf("password leaked in response: %s", body)
	}
	src := mustUnmarshal[Source](t, body)
	if src.Name != "main" || src.State != "ready" || src.Generation != 1 {
		t.Fatalf("source %+v", src)
	}

	code, body = do(t, ts, testToken, "GET", "/v1/sources", nil)
	if code != http.StatusOK {
		t.Fatalf("list sources: code=%d", code)
	}
	if list := mustUnmarshal[[]Source](t, body); len(list) != 1 {
		t.Fatalf("sources=%v", list)
	}

	code, body = do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	if code != http.StatusCreated {
		t.Fatalf("create branch: code=%d body=%s", code, body)
	}
	b := mustUnmarshal[Branch](t, body)
	if b.Name != "pr-1" || b.Source != "main" || b.State != "ready" || b.Port != 54321 {
		t.Fatalf("branch %+v", b)
	}
	if b.Host != "127.0.0.1" {
		t.Fatalf("branch host %q want 127.0.0.1", b.Host)
	}
	if b.ProxyDatabase != "postgres@pr-1" {
		t.Fatalf("proxy hint %q", b.ProxyDatabase)
	}
	if b.ExpiresAt != "" {
		t.Fatalf("no-ttl branch has expiry %q", b.ExpiresAt)
	}

	code, body = do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
	if code != http.StatusOK {
		t.Fatalf("get branch: code=%d", code)
	}
	code, body = do(t, ts, testToken, "GET", "/v1/branches", nil)
	if code != http.StatusOK {
		t.Fatalf("list branches: code=%d", code)
	}
	if list := mustUnmarshal[[]Branch](t, body); len(list) != 1 {
		t.Fatalf("branches=%v", list)
	}

	if code, body = do(t, ts, testToken, "DELETE", "/v1/branches/pr-1", nil); code != http.StatusNoContent {
		t.Fatalf("destroy branch: code=%d body=%s", code, body)
	}
	if code, _ = do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil); code != http.StatusNotFound {
		t.Fatalf("destroyed branch still resolves: code=%d", code)
	}
}

// Branch responses carry `password` only when the engine rotates per-branch
// credentials; inherit mode (default) must not even render the key.
func TestBranchPasswordOnlyInRotateMode(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)

	// rotate mode: create/get/list/reset all carry the rotated password
	ts, _ := newTestServer(t, engine.WithCredentialRotation())
	addSource(t, ts)
	code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	if code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	b := mustUnmarshal[Branch](t, body)
	if !hex32.MatchString(b.Password) {
		t.Fatalf("create password=%q want 32 hex chars", b.Password)
	}
	created := b.Password
	code, body = do(t, ts, testToken, "GET", "/v1/branches/pr-1", nil)
	if code != http.StatusOK {
		t.Fatalf("get: code=%d", code)
	}
	if got := mustUnmarshal[Branch](t, body); got.Password != created {
		t.Fatalf("get password=%q want %q", got.Password, created)
	}
	code, body = do(t, ts, testToken, "GET", "/v1/branches", nil)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d", code)
	}
	if list := mustUnmarshal[[]Branch](t, body); len(list) != 1 || list[0].Password != created {
		t.Fatalf("list password: %+v", list)
	}
	// reset re-rotates
	code, body = do(t, ts, testToken, "POST", "/v1/branches/pr-1/reset", nil)
	if code != http.StatusOK {
		t.Fatalf("reset: code=%d body=%s", code, body)
	}
	if got := mustUnmarshal[Branch](t, body); !hex32.MatchString(got.Password) || got.Password == created {
		t.Fatalf("reset password=%q (created %q) want a fresh 32-hex secret", got.Password, created)
	}

	// inherit mode: the key is absent entirely (omitempty)
	ts2, _ := newTestServer(t)
	addSource(t, ts2)
	code, body = do(t, ts2, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	if code != http.StatusCreated {
		t.Fatalf("create (inherit): code=%d body=%s", code, body)
	}
	if strings.Contains(string(body), `"password"`) {
		t.Fatalf("inherit mode rendered a password key: %s", body)
	}
}

func TestCreateBranchTTLPropagation(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	code, body := do(t, ts, testToken, "POST", "/v1/branches",
		CreateBranchRequest{Name: "pr-1", Source: "main", TTLSeconds: 3600})
	if code != http.StatusCreated {
		t.Fatalf("code=%d body=%s", code, body)
	}
	b := mustUnmarshal[Branch](t, body)
	got, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at=%q: %v", b.ExpiresAt, err)
	}
	want := time.Now().Add(time.Hour).UTC()
	if diff := got.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("expires_at=%s want ~%s", got, want)
	}
}

// TestCreateBranchQuotaReturns403 wires --max-branches via WithMaxBranches and
// asserts the over-cap create maps to HTTP 403 (writeEngineError's
// ErrQuotaExceeded branch).
func TestCreateBranchQuotaReturns403(t *testing.T) {
	ts, _ := newTestServer(t, engine.WithMaxBranches(1))
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches",
		CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("first create: code=%d body=%s", code, body)
	}
	code, body := do(t, ts, testToken, "POST", "/v1/branches",
		CreateBranchRequest{Name: "pr-2", Source: "main"})
	if code != http.StatusForbidden {
		t.Fatalf("over-cap create: code=%d body=%s, want 403", code, body)
	}
}

func TestResetBranchEndpoint(t *testing.T) {
	ts, d := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	code, body := do(t, ts, testToken, "POST", "/v1/branches/pr-1/reset", nil)
	if code != http.StatusOK {
		t.Fatalf("reset: code=%d body=%s", code, body)
	}
	b := mustUnmarshal[Branch](t, body)
	if b.State != "ready" {
		t.Fatalf("after reset: %+v", b)
	}
	if d.starts != 2 {
		t.Fatalf("starts=%d want 2 (container recreated)", d.starts)
	}
}

func TestDuplicateBranchConflict(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"})
	if code != http.StatusConflict {
		t.Fatalf("duplicate: code=%d body=%s", code, body)
	}
}

func TestNotFounds(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/branches/nope"},
		{"DELETE", "/v1/branches/nope"},
		{"POST", "/v1/branches/nope/reset"},
		{"DELETE", "/v1/sources/nope"},
		{"POST", "/v1/sources/nope/refresh"},
	} {
		var body any
		if c.method == "POST" && strings.HasSuffix(c.path, "refresh") {
			body = RefreshSourceRequest{Password: "x"}
		}
		code, resp := do(t, ts, testToken, c.method, c.path, body)
		if code != http.StatusNotFound {
			t.Errorf("%s %s: code=%d body=%s", c.method, c.path, code, resp)
		}
	}
}

func TestSourceRefreshAndRemove(t *testing.T) {
	ts, d := newTestServer(t)
	addSource(t, ts)

	code, body := do(t, ts, testToken, "POST", "/v1/sources/main/refresh", RefreshSourceRequest{Password: "secret"})
	if code != http.StatusOK {
		t.Fatalf("refresh: code=%d body=%s", code, body)
	}
	if s := mustUnmarshal[Source](t, body); s.Generation != 2 {
		t.Fatalf("generation=%d want 2", s.Generation)
	}

	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}
	// refused while a live branch exists
	code, body = do(t, ts, testToken, "DELETE", "/v1/sources/main", nil)
	if code != http.StatusConflict {
		t.Fatalf("remove with live branch: code=%d body=%s", code, body)
	}
	if code, body := do(t, ts, testToken, "DELETE", "/v1/branches/pr-1", nil); code != http.StatusNoContent {
		t.Fatalf("destroy: code=%d body=%s", code, body)
	}
	if code, body := do(t, ts, testToken, "DELETE", "/v1/sources/main", nil); code != http.StatusNoContent {
		t.Fatalf("remove source: code=%d body=%s", code, body)
	}
	if len(d.volumes) != 0 {
		t.Fatalf("volumes leaked: %v", d.volumes)
	}
}

func TestMaskScriptsPutGetAndApply(t *testing.T) {
	ts, d := newTestServer(t)
	addSource(t, ts)

	scripts := []MaskScript{
		{Name: "emails.sql", SQL: "UPDATE users SET email = 'x@invalid'"},
		{Name: "names.sql", SQL: "UPDATE users SET name = 'redacted'"},
	}
	code, body := do(t, ts, testToken, "PUT", "/v1/sources/main/mask", scripts)
	if code != http.StatusOK {
		t.Fatalf("put mask: code=%d body=%s", code, body)
	}
	code, body = do(t, ts, testToken, "GET", "/v1/sources/main/mask", nil)
	if code != http.StatusOK {
		t.Fatalf("get mask: code=%d body=%s", code, body)
	}
	got := mustUnmarshal[[]MaskScript](t, body)
	if len(got) != 2 || got[0] != scripts[0] || got[1] != scripts[1] {
		t.Fatalf("got %v want %v", got, scripts)
	}

	// PUT replaces all scripts
	code, body = do(t, ts, testToken, "PUT", "/v1/sources/main/mask", scripts[:1])
	if code != http.StatusOK {
		t.Fatalf("put replace: code=%d body=%s", code, body)
	}
	code, body = do(t, ts, testToken, "GET", "/v1/sources/main/mask", nil)
	if code != http.StatusOK {
		t.Fatalf("get after replace: code=%d", code)
	}
	if got = mustUnmarshal[[]MaskScript](t, body); len(got) != 1 || got[0] != scripts[0] {
		t.Fatalf("after replace: %v", got)
	}

	// branch creation applies the script via in-container psql
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create branch: code=%d body=%s", code, body)
	}
	var masked bool
	for _, c := range d.execs {
		if len(c) > 0 && c[0] == "psql" && c[len(c)-1] == scripts[0].SQL {
			masked = true
		}
	}
	if !masked {
		t.Fatalf("masking psql exec not recorded: %v", d.execs)
	}
}

func TestMaskScriptsUnknownSource(t *testing.T) {
	ts, _ := newTestServer(t)
	if code, body := do(t, ts, testToken, "PUT", "/v1/sources/nope/mask", []MaskScript{{Name: "a", SQL: "SELECT 1"}}); code != http.StatusNotFound {
		t.Fatalf("put unknown source: code=%d body=%s", code, body)
	}
	if code, body := do(t, ts, testToken, "GET", "/v1/sources/nope/mask", nil); code != http.StatusNotFound {
		t.Fatalf("get unknown source: code=%d body=%s", code, body)
	}
}

func TestBranchUsageEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}

	code, body := do(t, ts, testToken, "GET", "/v1/branches/pr-1/usage", nil)
	if code != http.StatusOK {
		t.Fatalf("usage: code=%d body=%s", code, body)
	}
	got := mustUnmarshal[map[string]int64](t, body)
	if got["bytes"] != 4096 { // the fake driver's canned du output
		t.Fatalf("usage = %v want bytes=4096", got)
	}

	if code, body := do(t, ts, testToken, "GET", "/v1/branches/nope/usage", nil); code != http.StatusNotFound {
		t.Fatalf("unknown branch usage: code=%d body=%s", code, body)
	}
	if code, _ := do(t, ts, "", "GET", "/v1/branches/pr-1/usage", nil); code != http.StatusUnauthorized {
		t.Fatalf("usage without token: code=%d want 401", code)
	}
}

func TestBadRequests(t *testing.T) {
	ts, _ := newTestServer(t)
	// malformed JSON
	req, _ := http.NewRequest("POST", ts.URL+"/v1/branches", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed json: code=%d", resp.StatusCode)
	}
	// missing required fields
	if code, _ := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "x"}); code != http.StatusBadRequest {
		t.Fatalf("missing source: code=%d", code)
	}
	if code, _ := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{Name: "x"}); code != http.StatusBadRequest {
		t.Fatalf("missing host: code=%d", code)
	}
}

func TestInvalidBranchNameBadRequest(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	for _, name := range []string{"PR-1", "pr_1", "-pr-1", strings.Repeat("a", 42)} {
		code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: name, Source: "main"})
		if code != http.StatusBadRequest {
			t.Errorf("name %q: code=%d body=%s, want 400", name, code, body)
		}
		e := mustUnmarshal[map[string]string](t, body)
		if !strings.Contains(e["error"], "invalid branch name") {
			t.Errorf("name %q: error %q", name, e["error"])
		}
	}
}

func TestCreateBranchFromParent(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "p", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create parent: code=%d body=%s", code, body)
	}

	code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "c", Parent: "p"})
	if code != http.StatusCreated {
		t.Fatalf("create from parent: code=%d body=%s", code, body)
	}
	b := mustUnmarshal[Branch](t, body)
	if b.Name != "c" || b.Parent != "p" || b.State != "ready" {
		t.Fatalf("child %+v", b)
	}
	if b.Source != "main" {
		t.Fatalf("child source=%q want main (inherited from parent)", b.Source)
	}
	if b.ProxyDatabase != "postgres@c" {
		t.Fatalf("proxy hint %q", b.ProxyDatabase)
	}

	// list carries the parent for every branch ("" for source-based)
	code, body = do(t, ts, testToken, "GET", "/v1/branches", nil)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d", code)
	}
	parents := map[string]string{}
	for _, br := range mustUnmarshal[[]Branch](t, body) {
		parents[br.Name] = br.Parent
	}
	if parents["p"] != "" || parents["c"] != "p" {
		t.Fatalf("list parents=%v", parents)
	}
}

func TestCreateBranchSourceParentMutuallyExclusive(t *testing.T) {
	ts, _ := newTestServer(t)
	addSource(t, ts)
	for name, req := range map[string]CreateBranchRequest{
		"both":    {Name: "x", Source: "main", Parent: "p"},
		"neither": {Name: "x"},
	} {
		code, body := do(t, ts, testToken, "POST", "/v1/branches", req)
		if code != http.StatusBadRequest {
			t.Errorf("%s: code=%d body=%s want 400", name, code, body)
		}
		e := mustUnmarshal[map[string]string](t, body)
		if !strings.Contains(e["error"], "source") || !strings.Contains(e["error"], "parent") {
			t.Errorf("%s: error %q should explain the source/parent rule", name, e["error"])
		}
	}
	// unknown parent -> 404
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "c", Parent: "ghost"}); code != http.StatusNotFound {
		t.Fatalf("unknown parent: code=%d body=%s want 404", code, body)
	}
}

func TestCreateSourceViaDumpRoundTrips(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.proj.supabase.co", Port: 5432, User: "postgres",
		Database: "appdb", PGVersion: "17", Password: "secret",
		Via: "dump", DumpSchemas: []string{"public", "audit"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create source: code=%d body=%s", code, body)
	}
	src := mustUnmarshal[Source](t, body)
	if src.Via != "dump" {
		t.Fatalf("via=%q want dump", src.Via)
	}
	if len(src.DumpSchemas) != 2 || src.DumpSchemas[0] != "public" || src.DumpSchemas[1] != "audit" {
		t.Fatalf("dump_schemas=%v want [public audit]", src.DumpSchemas)
	}
	// the list endpoint carries the method too
	code, body = do(t, ts, testToken, "GET", "/v1/sources", nil)
	if code != http.StatusOK {
		t.Fatalf("list: code=%d", code)
	}
	list := mustUnmarshal[[]Source](t, body)
	if len(list) != 1 || list[0].Via != "dump" || len(list[0].DumpSchemas) != 2 {
		t.Fatalf("listed source: %+v", list)
	}
}

func TestCreateSourceDefaultsToBasebackup(t *testing.T) {
	ts, _ := newTestServer(t)
	src := addSource(t, ts) // sends no via
	if src.Via != "basebackup" {
		t.Fatalf("via=%q want basebackup default", src.Via)
	}
	if len(src.DumpSchemas) != 0 {
		t.Fatalf("dump_schemas=%v want empty", src.DumpSchemas)
	}
}

func TestCreateSourceInvalidViaRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Password: "secret", Via: "rsync",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (body=%s)", code, body)
	}
	if !strings.Contains(string(body), "rsync") || !strings.Contains(string(body), "dump") {
		t.Errorf("body %s should name the invalid value and the valid ones", body)
	}
	// dump_schemas without via=dump is inconsistent input
	code, body = do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "main", Host: "db.internal", Password: "secret", DumpSchemas: []string{"public"},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("dump_schemas without via=dump: code=%d want 400 (body=%s)", code, body)
	}
}

func TestCreateSourceUnsupportedPGVersionRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	code, body := do(t, ts, testToken, "POST", "/v1/sources", CreateSourceRequest{
		Name: "old", Host: "db.internal", PGVersion: "13", Password: "secret",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (body=%s)", code, body)
	}
	if !strings.Contains(string(body), "unsupported pg_version") {
		t.Errorf("body %s should explain the unsupported version", body)
	}
}

func TestBranchDiffEndpoint(t *testing.T) {
	ts, d := newTestServer(t)
	addSource(t, ts)
	if code, body := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-1", Source: "main"}); code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", code, body)
	}

	code, body := do(t, ts, testToken, "GET", "/v1/branches/pr-1/diff", nil)
	if code != http.StatusOK {
		t.Fatalf("diff: code=%d body=%s", code, body)
	}
	got := mustUnmarshal[engine.DiffResult](t, body)
	// the fake driver serves the base dump without the "added" table
	if !strings.Contains(got.SchemaDiff, "+CREATE TABLE added (x integer);\n") {
		t.Errorf("schema_diff missing the added table:\n%s", got.SchemaDiff)
	}
	want := []engine.TableDelta{
		{Table: "added", BaseRows: 0, BranchRows: 7, Delta: 7},
		{Table: "users", BaseRows: 100, BranchRows: 100, Delta: 0},
	}
	if len(got.Tables) != len(want) {
		t.Fatalf("tables = %+v, want %+v", got.Tables, want)
	}
	for i := range want {
		g := got.Tables[i]
		if g.Table != want[i].Table || g.BaseRows != want[i].BaseRows ||
			g.BranchRows != want[i].BranchRows || g.Delta != want[i].Delta || g.SampleRows != nil {
			t.Errorf("tables[%d] = %+v, want %+v", i, g, want[i])
		}
	}
	// raw JSON shape (field names are the wire contract)
	for _, field := range []string{`"schema_diff"`, `"table"`, `"base_rows"`, `"branch_rows"`, `"delta"`} {
		if !strings.Contains(string(body), field) {
			t.Errorf("diff JSON missing %s: %s", field, body)
		}
	}

	if code, body := do(t, ts, testToken, "GET", "/v1/branches/nope/diff", nil); code != http.StatusNotFound {
		t.Fatalf("unknown branch diff: code=%d body=%s", code, body)
	}
	if code, _ := do(t, ts, "", "GET", "/v1/branches/pr-1/diff", nil); code != http.StatusUnauthorized {
		t.Fatalf("diff without token: code=%d want 401", code)
	}

	// ?data=N turns on bounded data sampling: the grown table "added" comes
	// back with its branch-only rows (x=1,2); without it SampleRows is absent.
	if strings.Contains(string(body), `"sample_rows"`) {
		t.Errorf("diff without ?data leaked sample_rows: %s", body)
	}
	code, body = do(t, ts, testToken, "GET", "/v1/branches/pr-1/diff?data=20", nil)
	if code != http.StatusOK {
		t.Fatalf("diff?data: code=%d body=%s", code, body)
	}
	sampled := mustUnmarshal[engine.DiffResult](t, body)
	var added *engine.TableDelta
	for i := range sampled.Tables {
		if sampled.Tables[i].Table == "added" {
			added = &sampled.Tables[i]
		}
	}
	if added == nil || len(added.SampleRows) != 2 {
		t.Fatalf("added sample rows = %+v, want 2 branch-only rows", added)
	}
	if !strings.Contains(string(body), `"sample_rows"`) {
		t.Errorf("diff?data did not emit sample_rows: %s", body)
	}
	// a bad data value is a 400
	if code, _ := do(t, ts, testToken, "GET", "/v1/branches/pr-1/diff?data=-3", nil); code != http.StatusBadRequest {
		t.Errorf("diff?data=-3: code=%d want 400", code)
	}

	// a non-ready branch is refused with 409
	d.failStart = true
	if code, _ := do(t, ts, testToken, "POST", "/v1/branches", CreateBranchRequest{Name: "pr-2", Source: "main"}); code == http.StatusCreated {
		t.Fatal("pr-2 create should have failed")
	}
	d.failStart = false
	if code, body := do(t, ts, testToken, "GET", "/v1/branches/pr-2/diff", nil); code != http.StatusConflict {
		t.Fatalf("non-ready branch diff: code=%d body=%s, want 409", code, body)
	}
}

// The reconcile endpoints report a plan (GET) and apply it (POST), both behind
// the bearer token. A stray managed volume with no registry row drifts; GET
// lists it, POST removes it.
func TestReconcileEndpoints(t *testing.T) {
	ts, d := newTestServer(t)
	addSource(t, ts)
	// a stray managed volume owned by no branch -> gc_volume drift.
	d.volumes["pgbranch-br-stray-rw"] = true

	// auth required.
	if code, _ := do(t, ts, "", "GET", "/v1/reconcile/plan", nil); code != http.StatusUnauthorized {
		t.Fatalf("plan without token: code=%d want 401", code)
	}
	if code, _ := do(t, ts, "", "POST", "/v1/reconcile", nil); code != http.StatusUnauthorized {
		t.Fatalf("apply without token: code=%d want 401", code)
	}

	// GET plan: reports the drift, mutates nothing.
	code, body := do(t, ts, testToken, "GET", "/v1/reconcile/plan", nil)
	if code != http.StatusOK {
		t.Fatalf("plan: code=%d body=%s", code, body)
	}
	plan := mustUnmarshal[engine.ReconcilePlan](t, body)
	if !plan.Drift() || !planHas(plan, engine.ActionGCVolume, "pgbranch-br-stray-rw") {
		t.Fatalf("plan missing stray-volume drift: %+v", plan.Actions)
	}
	if !d.volumes["pgbranch-br-stray-rw"] {
		t.Fatal("GET /v1/reconcile/plan mutated state")
	}
	// wire-contract field names.
	for _, field := range []string{`"actions"`, `"kind"`, `"target"`, `"reason"`} {
		if !strings.Contains(string(body), field) {
			t.Errorf("plan JSON missing %s: %s", field, body)
		}
	}

	// POST apply: removes the stray volume, returns the action taken.
	code, body = do(t, ts, testToken, "POST", "/v1/reconcile", nil)
	if code != http.StatusOK {
		t.Fatalf("apply: code=%d body=%s", code, body)
	}
	taken := mustUnmarshal[engine.ReconcilePlan](t, body)
	if !planHas(taken, engine.ActionGCVolume, "pgbranch-br-stray-rw") {
		t.Fatalf("apply did not report removing the stray volume: %+v", taken.Actions)
	}
	if d.volumes["pgbranch-br-stray-rw"] {
		t.Fatal("apply did not remove the stray volume")
	}
}

func planHas(p engine.ReconcilePlan, kind engine.ActionKind, target string) bool {
	for _, a := range p.Actions {
		if a.Kind == kind && a.Target == target {
			return true
		}
	}
	return false
}
