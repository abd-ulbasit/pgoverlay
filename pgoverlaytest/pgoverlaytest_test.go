package pgoverlaytest

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
	reason    string // reason of the last /history transition
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
	mux.HandleFunc("GET /v1/branches/{name}/history", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		json.NewEncoder(w).Encode([]map[string]string{
			{"from_state": "", "to_state": "creating", "reason": "create"},
			{"from_state": "creating", "to_state": s.getStates[0], "reason": s.reason},
		})
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
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	t.Setenv("PGOVERLAY_TEST_SOURCE", "")
	t.Setenv("PGOVERLAY_PASSWORD", "")

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
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	t.Setenv("PGOVERLAY_TEST_SOURCE", "env-source")

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
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	t.Setenv("PGOVERLAY_PASSWORD", "s3cr:t/pw")

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })

	if b.Host != "10.0.0.7" || b.Port != 31234 || b.User != "appuser" || b.Database != "appdb" {
		t.Fatalf("branch = %+v", b)
	}
	if b.Password != "s3cr:t/pw" {
		t.Errorf("password = %q, want env fallback", b.Password)
	}
	wantDSN := "postgres://appuser:s3cr%3At%2Fpw@10.0.0.7:31234/appdb"
	if b.DSN != wantDSN {
		t.Errorf("DSN = %q, want %q", b.DSN, wantDSN)
	}
	u, err := url.Parse(stub.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	wantProxy := fmt.Sprintf("postgres://appuser:s3cr%%3At%%2Fpw@%s:6432/appdb@%s", u.Hostname(), b.Name)
	if b.ProxyDSN != wantProxy {
		t.Errorf("ProxyDSN = %q, want %q", b.ProxyDSN, wantProxy)
	}
}

// TestDSNRoundTrip: whatever the credentials, the DSN must parse back (as pgx
// parses postgres:// URLs, via net/url) to exactly the same user, password,
// host and database. url.QueryEscape got this wrong: it encodes a space as
// '+', which userinfo parsing keeps as a literal '+'.
func TestDSNRoundTrip(t *testing.T) {
	tests := []struct {
		user, password, host, db string
	}{
		{"postgres", "pass word", "10.0.0.7", "postgres"},
		{"app user", `p@ss:w/rd?#+%&= "x"`, "db.example.com", "appdb"},
		{"postgres", "", "127.0.0.1", "postgres@t-foo-abc123"},
		{"postgres", "pw", "fd00::7", "appdb@t-bar"},
		{"postgres", "pw", "::1", "postgres"},
	}
	for _, tt := range tests {
		got := dsn(tt.user, tt.password, tt.host, 5432, tt.db)
		u, err := url.Parse(got)
		if err != nil {
			t.Errorf("dsn(%q, %q, %q) = %q: does not parse: %v", tt.user, tt.password, tt.host, got, err)
			continue
		}
		if strings.Contains(got, "+") && !strings.Contains(tt.password, "+") {
			t.Errorf("dsn = %q: a '+' is not a space in URL userinfo", got)
		}
		if u.User.Username() != tt.user {
			t.Errorf("dsn = %q: user parses as %q, want %q", got, u.User.Username(), tt.user)
		}
		pw, set := u.User.Password()
		if pw != tt.password || set != (tt.password != "") {
			t.Errorf("dsn = %q: password parses as %q (set=%v), want %q", got, pw, set, tt.password)
		}
		if u.Hostname() != tt.host || u.Port() != "5432" {
			t.Errorf("dsn = %q: host parses as %q port %q, want %q 5432", got, u.Hostname(), u.Port(), tt.host)
		}
		if strings.TrimPrefix(u.Path, "/") != tt.db {
			t.Errorf("dsn = %q: database parses as %q, want %q", got, u.Path, tt.db)
		}
	}
}

func TestSplitProxyHost(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"pgoverlay-proxy.pgoverlay-system", "pgoverlay-proxy.pgoverlay-system", 6432, false},
		{"proxy.example.com:30432", "proxy.example.com", 30432, false},
		{"10.0.0.9:6433", "10.0.0.9", 6433, false},
		{"fd00::1", "fd00::1", 6432, false},
		{"[fd00::1]", "fd00::1", 6432, false},
		{"[fd00::1]:7432", "fd00::1", 7432, false},
		{"proxy:", "", 0, true},
		{"proxy:notaport", "", 0, true},
		{"proxy:70000", "", 0, true},
		{":6432", "", 0, true},
		{"[]", "", 0, true},
		{"http://proxy", "", 0, true},
	}
	for _, tt := range tests {
		h, p, err := splitProxyHost(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("splitProxyHost(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if h != tt.wantHost || p != tt.wantPort {
			t.Errorf("splitProxyHost(%q) = %q, %d; want %q, %d", tt.in, h, p, tt.wantHost, tt.wantPort)
		}
	}
}

// TestAcquireProxyHost: ProxyDSN targets PGOVERLAY_PROXY_HOST / WithProxyHost
// when set (the Helm chart's separate proxy Service), not the API host.
func TestAcquireProxyHost(t *testing.T) {
	stub := newStub(t)
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	t.Setenv("PGOVERLAY_PASSWORD", "pw")

	proxyAddr := func(b *Branch) string {
		t.Helper()
		u, err := url.Parse(b.ProxyDSN)
		if err != nil {
			t.Fatalf("ProxyDSN %q: %v", b.ProxyDSN, err)
		}
		if want := "/appdb@" + b.Name; u.Path != want {
			t.Errorf("ProxyDSN path = %q, want %q", u.Path, want)
		}
		return u.Host
	}

	t.Setenv("PGOVERLAY_PROXY_HOST", "pgoverlay-proxy.pgoverlay-system:7432")
	var b *Branch
	t.Run("env", func(t *testing.T) { b = Acquire(t) })
	if got := proxyAddr(b); got != "pgoverlay-proxy.pgoverlay-system:7432" {
		t.Errorf("env proxy host: ProxyDSN host = %q", got)
	}

	t.Run("option wins, default port", func(t *testing.T) { b = Acquire(t, WithProxyHost("proxy.internal")) })
	if got := proxyAddr(b); got != "proxy.internal:6432" {
		t.Errorf("WithProxyHost: ProxyDSN host = %q, want proxy.internal:6432", got)
	}

	t.Run("ipv6", func(t *testing.T) { b = Acquire(t, WithProxyHost("[fd00::1]:6433")) })
	if got := proxyAddr(b); got != "[fd00::1]:6433" {
		t.Errorf("IPv6 proxy host: ProxyDSN host = %q, want [fd00::1]:6433", got)
	}

	// a malformed override fails before any branch is created
	stub.mu.Lock()
	creates := len(stub.creates)
	stub.mu.Unlock()
	f := &fakeTB{name: "TestAcquireProxyHost"}
	runWithFakeTB(f, func() { Acquire(f, WithProxyHost("proxy:notaport")) })
	if !f.failed || !strings.Contains(f.msg, "proxy:notaport") {
		t.Fatalf("Acquire with a bad proxy host: failed=%v msg=%q", f.failed, f.msg)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.creates) != creates {
		t.Errorf("a branch was created despite the invalid proxy host")
	}
}

// TestAcquireIPv6DirectHost: a server reporting an IPv6 branch host yields a
// bracketed, parseable DSN.
func TestAcquireIPv6DirectHost(t *testing.T) {
	stub := newStub(t)
	stub.branch.Host = "fd00::7"
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	t.Setenv("PGOVERLAY_PASSWORD", "pw")

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })
	if want := "postgres://appuser:pw@[fd00::7]:31234/appdb"; b.DSN != want {
		t.Errorf("DSN = %q, want %q", b.DSN, want)
	}
}

func TestTTLSeconds(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want int
	}{
		{0, 0},
		{time.Nanosecond, 1},
		{500 * time.Millisecond, 1},
		{time.Second, 1},
		{1500 * time.Millisecond, 2},
		{2 * time.Minute, 120},
		{time.Hour, 3600},
	}
	for _, tt := range tests {
		if got := ttlSeconds(tt.d); got != tt.want {
			t.Errorf("ttlSeconds(%s) = %d, want %d", tt.d, got, tt.want)
		}
	}
}

// TestAcquireTTLEdges: a sub-second TTL keeps the safety net (rounded up to
// 1s instead of truncated to 0 = never reaped), and a negative TTL fails
// before any branch is created.
func TestAcquireTTLEdges(t *testing.T) {
	stub := newStub(t)
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")

	t.Run("sub-second", func(t *testing.T) { Acquire(t, WithTTL(500*time.Millisecond)) })
	if got := stub.lastCreate().TTLSeconds; got != 1 {
		t.Errorf("ttl_seconds for 500ms = %d, want 1", got)
	}

	t.Run("zero", func(t *testing.T) { Acquire(t, WithTTL(0)) })
	if got := stub.lastCreate().TTLSeconds; got != 0 {
		t.Errorf("ttl_seconds for 0 = %d, want 0", got)
	}

	stub.mu.Lock()
	creates := len(stub.creates)
	stub.mu.Unlock()
	f := &fakeTB{name: "TestAcquireTTLEdges"}
	runWithFakeTB(f, func() { Acquire(f, WithTTL(-time.Second)) })
	if !f.failed {
		t.Fatal("Acquire accepted a negative TTL")
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.creates) != creates {
		t.Errorf("a branch was created despite the negative TTL")
	}
}

// TestAcquireWirePassword: a server that returns a per-branch password (rotate
// mode, future) wins over the env fallback.
func TestAcquireWirePassword(t *testing.T) {
	stub := newStub(t)
	stub.branch.Password = "rotated-pw"
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	t.Setenv("PGOVERLAY_PASSWORD", "env-pw")

	var b *Branch
	t.Run("inner", func(t *testing.T) { b = Acquire(t) })
	if b.Password != "rotated-pw" {
		t.Errorf("password = %q, want rotated-pw (wire field wins)", b.Password)
	}
}

func TestAcquirePollsUntilReady(t *testing.T) {
	stub := newStub(t)
	stub.getStates = []string{"creating", "creating", "ready"}
	t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")

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
	msg      string
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
	f.msg = fmt.Sprintf(format, args...)
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
	t.Setenv("PGOVERLAY_SERVER", "")
	f := &fakeTB{name: "TestAcquireSkipsWithoutServer"}
	runWithFakeTB(f, func() { Acquire(f) })
	if !f.skipped {
		t.Fatal("Acquire did not skip with PGOVERLAY_SERVER unset")
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
	t.Setenv("PGOVERLAY_SERVER", ts.URL)
	t.Setenv("PGOVERLAY_TOKEN", "tok-1")
	f := &fakeTB{name: "TestAcquireFailsOnServerError"}
	runWithFakeTB(f, func() { Acquire(f) })
	if !f.failed {
		t.Fatal("Acquire did not fail on a 500 create")
	}
}

// TestAcquireFailsFastOnTerminalState: a branch that turns failed, destroying
// or destroyed can never become ready. Acquire must fail at once with the
// server's reason — not poll until the 5-minute deadline — and still destroy
// the branch through the cleanup it registered.
func TestAcquireFailsFastOnTerminalState(t *testing.T) {
	old := pollInterval
	pollInterval = time.Millisecond
	defer func() { pollInterval = old }()

	for _, state := range []string{"failed", "destroying", "destroyed"} {
		t.Run(state, func(t *testing.T) {
			stub := newStub(t)
			stub.getStates = []string{"creating", state}
			stub.reason = "clone failed: no space left on device"
			t.Setenv("PGOVERLAY_SERVER", stub.ts.URL)
			t.Setenv("PGOVERLAY_TOKEN", "tok-1")

			f := &fakeTB{name: "TestTerminal"}
			done := make(chan struct{})
			go func() {
				defer close(done)
				runWithFakeTB(f, func() { Acquire(f) })
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("Acquire kept polling a %s branch", state)
			}
			if !f.failed {
				t.Fatalf("Acquire did not fail on a %s branch", state)
			}
			if !strings.Contains(f.msg, state) || !strings.Contains(f.msg, stub.reason) {
				t.Errorf("failure message %q lacks the state %q or the server's reason", f.msg, state)
			}
			stub.mu.Lock()
			gets := stub.gets
			stub.mu.Unlock()
			// the stub replays "creating" on the first GET, then reports state
			if gets != 2 {
				t.Errorf("GET polls = %d, want 2 (stop at the first terminal state)", gets)
			}
			if len(f.cleanups) != 1 {
				t.Fatalf("cleanups = %d, want 1 (destroy registered before the ready-wait)", len(f.cleanups))
			}
			f.cleanups[0]()
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if len(stub.deletes) != 1 {
				t.Errorf("deletes = %v, want the failed branch destroyed", stub.deletes)
			}
		})
	}
}
