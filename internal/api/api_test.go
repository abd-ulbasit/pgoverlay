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
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
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
func (f *fakeDriver) RunHelper(ctx context.Context, s runtime.HelperSpec) error { return nil }
func (f *fakeDriver) StartBranch(ctx context.Context, s runtime.BranchSpec) (string, error) {
	if f.failStart {
		return "", errors.New("boom")
	}
	f.starts++
	f.containers["cid-"+s.Name] = true
	return "cid-" + s.Name, nil
}
func (f *fakeDriver) Exec(ctx context.Context, id string, cmd []string) error { return nil }
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

const testToken = "sekrit"

func newTestServer(t *testing.T) (*httptest.Server, *fakeDriver) {
	t.Helper()
	d := newFake()
	reg, err := registry.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	eng := engine.New(reg, d, "postgres:17")
	ts := httptest.NewServer(New(eng, reg, testToken).Handler())
	t.Cleanup(ts.Close)
	return ts, d
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
