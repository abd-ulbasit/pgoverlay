package pgbranchtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func TestBranchName(t *testing.T) {
	const suffix = "abc123"
	tests := []struct {
		testName string
		want     string
	}{
		{"TestFoo", "t-testfoo-abc123"},
		{"TestFoo/sub_case", "t-testfoo-sub-case-abc123"},
		{"Test__Weird--Chars!!", "t-test-weird-chars-abc123"},
		// 40 a's: left-truncated to the trailing 32 chars
		{strings.Repeat("a", 40), "t-" + strings.Repeat("a", 32) + "-abc123"},
		// truncation point lands on a separator: leading dash must be trimmed
		{strings.Repeat("x", 31) + "_" + strings.Repeat("y", 31), "t-" + strings.Repeat("y", 31) + "-abc123"},
		// nothing sanitizable left
		{"!!!", "t-abc123"},
		{"", "t-abc123"},
	}
	for _, tt := range tests {
		got := branchName(tt.testName, suffix)
		if got != tt.want {
			t.Errorf("branchName(%q) = %q, want %q", tt.testName, got, tt.want)
		}
		if len(got) > 41 {
			t.Errorf("branchName(%q) = %q: %d chars, want <= 41", tt.testName, got, len(got))
		}
		if !nameRe.MatchString(got) {
			t.Errorf("branchName(%q) = %q: does not match %s", tt.testName, got, nameRe)
		}
		// truncation keeps the test name's tail, not its head
		if !strings.HasSuffix(got, "-"+suffix) && got != "t-"+suffix {
			t.Errorf("branchName(%q) = %q: random suffix lost", tt.testName, got)
		}
	}
}

func TestBranchNameRandomSuffix(t *testing.T) {
	a := randHex(6)
	b := randHex(6)
	if len(a) != 6 || len(b) != 6 {
		t.Fatalf("randHex(6) lengths: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("randHex returned identical values %q (not random?)", a)
	}
	if !regexp.MustCompile(`^[0-9a-f]{6}$`).MatchString(a) {
		t.Fatalf("randHex(6) = %q, want lowercase hex", a)
	}
}

// stubServer is a minimal in-memory branchd: records requests, returns a
// configurable sequence of states from GET.
type stubServer struct {
	t         *testing.T
	mu        sync.Mutex
	creates   []createBranchRequest
	gets      int
	deletes   []string
	auths     []string
	getStates []string // consumed one per GET; last one repeats
	branch    wireBranch
	ts        *httptest.Server
}

func newStub(t *testing.T) *stubServer {
	s := &stubServer{t: t, getStates: []string{"ready"}}
	s.branch = wireBranch{
		State: "ready", Host: "10.0.0.7", Port: 31234,
		User: "appuser", Database: "appdb",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/branches", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		var req createBranchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("stub: bad create body: %v", err)
		}
		s.creates = append(s.creates, req)
		b := s.branch
		b.Name = req.Name
		b.Source = req.Source
		b.ProxyDatabase = b.Database + "@" + req.Name
		b.State = s.getStates[0]
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("GET /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		state := s.getStates[0]
		if len(s.getStates) > 1 {
			s.getStates = s.getStates[1:]
		}
		s.gets++
		b := s.branch
		b.Name = r.PathValue("name")
		b.ProxyDatabase = b.Database + "@" + b.Name
		b.State = state
		json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("DELETE /v1/branches/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.deletes = append(s.deletes, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	s.ts = httptest.NewServer(mux)
	t.Cleanup(s.ts.Close)
	return s
}

func (s *stubServer) lastCreate() createBranchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.creates) == 0 {
		s.t.Fatal("stub: no create request received")
	}
	return s.creates[len(s.creates)-1]
}

func TestAcquireDefaults(t *testing.T) {
	stub := newStub(t)
	t.Setenv("PGBRANCH_SERVER", stub.ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok-1")
	t.Setenv("PGBRANCH_TEST_SOURCE", "")
	t.Setenv("PGBRANCH_PASSWORD", "")

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })

	req := stub.lastCreate()
	if req.Source != "main" {
		t.Errorf("default source = %q, want main", req.Source)
	}
	if req.TTLSeconds != 3600 {
		t.Errorf("default ttl_seconds = %d, want 3600", req.TTLSeconds)
	}
	if req.Name != b.Name || !nameRe.MatchString(b.Name) || !strings.HasPrefix(b.Name, "t-") {
		t.Errorf("branch name %q (request %q)", b.Name, req.Name)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, a := range stub.auths {
		if a != "Bearer tok-1" {
			t.Errorf("auth header = %q, want Bearer tok-1", a)
		}
	}
	// the inner test ended: its cleanup must have destroyed the branch
	if len(stub.deletes) != 1 || stub.deletes[0] != b.Name {
		t.Errorf("deletes = %v, want [%s]", stub.deletes, b.Name)
	}
}

func TestAcquireOptions(t *testing.T) {
	stub := newStub(t)
	t.Setenv("PGBRANCH_SERVER", stub.ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok-1")
	t.Setenv("PGBRANCH_TEST_SOURCE", "env-source")

	t.Run("env source wins over default", func(t *testing.T) { Acquire(t) })
	if got := stub.lastCreate().Source; got != "env-source" {
		t.Errorf("source = %q, want env-source", got)
	}

	t.Run("explicit options win", func(t *testing.T) {
		Acquire(t, WithSource("explicit"), WithTTL(2*time.Minute))
	})
	req := stub.lastCreate()
	if req.Source != "explicit" {
		t.Errorf("source = %q, want explicit", req.Source)
	}
	if req.TTLSeconds != 120 {
		t.Errorf("ttl_seconds = %d, want 120", req.TTLSeconds)
	}
}

func TestAcquireBranchFields(t *testing.T) {
	stub := newStub(t)
	t.Setenv("PGBRANCH_SERVER", stub.ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok-1")
	t.Setenv("PGBRANCH_PASSWORD", "s3cr:t/pw")

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })

	if b.Host != "10.0.0.7" || b.Port != 31234 || b.User != "appuser" || b.Database != "appdb" {
		t.Fatalf("branch = %+v", b)
	}
	if b.Password != "s3cr:t/pw" {
		t.Errorf("password = %q, want env fallback", b.Password)
	}
	wantDSN := fmt.Sprintf("postgres://appuser:%s@10.0.0.7:31234/appdb", url.QueryEscape("s3cr:t/pw"))
	if b.DSN != wantDSN {
		t.Errorf("DSN = %q, want %q", b.DSN, wantDSN)
	}
	u, err := url.Parse(stub.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	wantProxy := fmt.Sprintf("postgres://appuser:%s@%s:6432/appdb@%s", url.QueryEscape("s3cr:t/pw"), u.Hostname(), b.Name)
	if b.ProxyDSN != wantProxy {
		t.Errorf("ProxyDSN = %q, want %q", b.ProxyDSN, wantProxy)
	}
}

// TestAcquireWirePassword: a server that returns a per-branch password (rotate
// mode, future) wins over the env fallback.
func TestAcquireWirePassword(t *testing.T) {
	stub := newStub(t)
	stub.branch.Password = "rotated-pw"
	t.Setenv("PGBRANCH_SERVER", stub.ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok-1")
	t.Setenv("PGBRANCH_PASSWORD", "env-pw")

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })
	if b.Password != "rotated-pw" {
		t.Errorf("password = %q, want rotated-pw (wire field wins)", b.Password)
	}
}

func TestAcquirePollsUntilReady(t *testing.T) {
	stub := newStub(t)
	stub.getStates = []string{"creating", "creating", "ready"}
	t.Setenv("PGBRANCH_SERVER", stub.ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok-1")

	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })
	if b == nil {
		t.Fatal("Acquire returned nil")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.gets < 2 {
		t.Errorf("GET polls = %d, want >= 2 (create returned creating)", stub.gets)
	}
}

// fakeTB observes Skip/Fatal behavior without killing the real test.
type fakeTB struct {
	testing.TB
	name     string
	skipped  bool
	failed   bool
	cleanups []func()
}

func (f *fakeTB) Helper()                          {}
func (f *fakeTB) Name() string                     { return f.name }
func (f *fakeTB) Logf(format string, args ...any)  {}
func (f *fakeTB) Cleanup(fn func())                { f.cleanups = append(f.cleanups, fn) }
func (f *fakeTB) Skip(args ...any)                 { f.skipped = true; runtime.Goexit() }
func (f *fakeTB) Skipf(format string, args ...any) { f.skipped = true; runtime.Goexit() }
func (f *fakeTB) Fatal(args ...any)                { f.failed = true; runtime.Goexit() }
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed = true
	runtime.Goexit()
}

func runWithFakeTB(f *fakeTB, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	<-done
}

func TestAcquireSkipsWithoutServer(t *testing.T) {
	t.Setenv("PGBRANCH_SERVER", "")
	f := &fakeTB{name: "TestAcquireSkipsWithoutServer"}
	runWithFakeTB(f, func() { Acquire(f) })
	if !f.skipped {
		t.Fatal("Acquire did not skip with PGBRANCH_SERVER unset")
	}
	if f.failed {
		t.Fatal("Acquire failed instead of skipping")
	}
}

func TestAcquireFailsOnServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("PGBRANCH_SERVER", ts.URL)
	t.Setenv("PGBRANCH_TOKEN", "tok-1")
	f := &fakeTB{name: "TestAcquireFailsOnServerError"}
	runWithFakeTB(f, func() { Acquire(f) })
	if !f.failed {
		t.Fatal("Acquire did not fail on a 500 create")
	}
}
