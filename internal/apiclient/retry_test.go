package apiclient

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
)

// fastRetry keeps the retry tests quick while exercising the same code path.
var fastRetry = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}

// scripted serves the given statuses in order (repeating the last one) and
// records each request's method and client address.
type scripted struct {
	mu       sync.Mutex
	statuses []int
	methods  []string
	remotes  []string
}

func (s *scripted) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := len(s.methods)
	s.methods = append(s.methods, r.Method)
	s.remotes = append(s.remotes, r.RemoteAddr)
	st := s.statuses[min(n, len(s.statuses)-1)]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	switch {
	case st == http.StatusServiceUnavailable:
		json.NewEncoder(w).Encode(map[string]string{"error": "not leader"})
	case st == http.StatusBadGateway || st == http.StatusGatewayTimeout:
		// what a proxy in front of branchd sends: not branchd's JSON error
		w.Write([]byte("<html><body>" + http.StatusText(st) + "</body></html>"))
	case st >= 400:
		json.NewEncoder(w).Encode(map[string]string{"error": http.StatusText(st)})
	default:
		json.NewEncoder(w).Encode(api.Branch{Name: "pr-1", State: "ready"})
	}
}

func (s *scripted) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.methods)
}

func newScripted(t *testing.T, statuses ...int) (*scripted, *Client) {
	t.Helper()
	s := &scripted{statuses: statuses}
	ts := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(ts.Close)
	c := New(ts.URL, "tok")
	c.Retry = fastRetry
	return s, c
}

// #19: a follower replica answers a mutation with 503 "not leader" before
// doing any work, so even a POST is retried, on a fresh connection (a Service
// balances per connection, so the retry can reach the leader).
func TestRetriesNotLeaderOnFreshConnection(t *testing.T) {
	s, c := newScripted(t, http.StatusServiceUnavailable, http.StatusCreated)
	b, err := c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1", Source: "main"})
	if err != nil {
		t.Fatalf("CreateBranch after one 503: %v", err)
	}
	if b.Name != "pr-1" || s.count() != 2 {
		t.Fatalf("branch %+v after %d requests, want pr-1 after 2", b, s.count())
	}
	if s.remotes[0] == s.remotes[1] {
		t.Fatalf("retry reused connection %s; want a new dial so a load balancer can pick another replica", s.remotes[0])
	}
}

func TestRetryGivesUpAfterMaxAttempts(t *testing.T) {
	s, c := newScripted(t, http.StatusServiceUnavailable)
	_, err := c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1", Source: "main"})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want a 503 *StatusError", err)
	}
	if !strings.Contains(err.Error(), "not leader") || !strings.Contains(err.Error(), "gave up after 4 attempts") {
		t.Fatalf("err = %q, want the server message and the attempt count", err)
	}
	if s.count() != fastRetry.MaxAttempts {
		t.Fatalf("%d requests, want %d", s.count(), fastRetry.MaxAttempts)
	}
}

// A 502/504 from a proxy may hide a request branchd already processed, so
// only idempotent methods are repeated.
func TestGatewayErrorsRetryOnlyIdempotentMethods(t *testing.T) {
	for _, st := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		s, c := newScripted(t, st, http.StatusOK)
		if _, err := c.GetBranch(context.Background(), "pr-1"); err != nil {
			t.Fatalf("GET after %d: %v", st, err)
		}
		if s.count() != 2 {
			t.Fatalf("GET after %d: %d requests, want 2", st, s.count())
		}

		s, c = newScripted(t, st, http.StatusCreated)
		if _, err := c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1"}); err == nil {
			t.Fatalf("POST after %d succeeded; it must not be repeated", st)
		}
		if s.count() != 1 {
			t.Fatalf("POST after %d: %d requests, want 1", st, s.count())
		}
	}
}

// branchd's own 504 means the operation ran into the stuck timeout and was
// rolled back; repeating even a GET (diff provisions a throwaway branch)
// would run it again for as long again, so it is final.
func TestBranchdGatewayTimeoutIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(map[string]string{"error": "operation exceeded the stuck timeout; the operation was cancelled and its partial work rolled back, retry it"})
	}))
	t.Cleanup(ts.Close)
	c := New(ts.URL, "tok")
	c.Retry = fastRetry
	_, err := c.GetBranch(context.Background(), "pr-1")
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusGatewayTimeout || !strings.Contains(err.Error(), "stuck timeout") {
		t.Fatalf("err = %v, want branchd's 504", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("%d requests, want 1: branchd's 504 must not be retried", n)
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	for _, st := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError} {
		s, c := newScripted(t, st, http.StatusOK)
		if _, err := c.GetBranch(context.Background(), "pr-1"); err == nil {
			t.Fatalf("GET with %d succeeded", st)
		}
		if s.count() != 1 {
			t.Fatalf("status %d: %d requests, want 1 (not retryable)", st, s.count())
		}
	}
}

// A refused connection means nothing was sent, so even a POST is retried.
func TestRetriesRefusedConnection(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	lis.Close() // nothing listens here now

	c := New("http://"+addr, "tok")
	c.Retry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	_, err = c.CreateBranch(context.Background(), api.CreateBranchRequest{Name: "pr-1"})
	if err == nil || !strings.Contains(err.Error(), "gave up after 3 attempts") {
		t.Fatalf("err = %v, want a dial error retried 3 times", err)
	}
}

func TestUnknownHostIsNotRetried(t *testing.T) {
	const host = "pgoverlay-no-such-host.invalid"
	var dns *net.DNSError
	if _, err := net.DefaultResolver.LookupHost(context.Background(), host); !errors.As(err, &dns) || !dns.IsNotFound {
		t.Skipf("resolver does not report NXDOMAIN for %s here (%v)", host, err)
	}
	c := New("http://"+host+":7070", "tok")
	c.Retry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
	_, err := c.ListBranches(context.Background())
	if err == nil || strings.Contains(err.Error(), "gave up") {
		t.Fatalf("err = %v, want an immediate DNS failure", err)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	s, c := newScripted(t, http.StatusServiceUnavailable)
	c.Retry = RetryPolicy{MaxAttempts: 10, BaseDelay: time.Hour, MaxDelay: time.Hour}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ListBranches(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
	if time.Since(start) > 5*time.Second || s.count() != 1 {
		t.Fatalf("took %s with %d requests; the backoff wait must end with the context", time.Since(start), s.count())
	}
}

func TestZeroRetryPolicySendsOnce(t *testing.T) {
	s, c := newScripted(t, http.StatusServiceUnavailable)
	c.Retry = RetryPolicy{}
	if _, err := c.ListBranches(context.Background()); err == nil {
		t.Fatal("want the 503")
	}
	if s.count() != 1 {
		t.Fatalf("%d requests, want 1", s.count())
	}
}

func TestBackoffIsBounded(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 10, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second}
	for attempt := 1; attempt <= 10; attempt++ {
		d := p.backoff(attempt, 0)
		if d <= 0 || d > p.MaxDelay {
			t.Fatalf("backoff(%d) = %s, want within (0, %s]", attempt, d, p.MaxDelay)
		}
	}
	if d := p.backoff(1, 30*time.Second); d != p.MaxDelay {
		t.Fatalf("Retry-After 30s gave %s, want it capped at %s", d, p.MaxDelay)
	}
	if d := p.backoff(1, 500*time.Millisecond); d != 500*time.Millisecond {
		t.Fatalf("Retry-After 500ms gave %s, want it honoured", d)
	}
}

func TestUnauthorizedWithoutTokenSaysSo(t *testing.T) {
	var gotAuth []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Values("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing or invalid bearer token"})
	}))
	t.Cleanup(ts.Close)
	_, err := New(ts.URL, "").ListBranches(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no bearer token was sent") {
		t.Fatalf("err = %v, want a hint that no token was sent", err)
	}
	if len(gotAuth) != 0 {
		t.Fatalf("sent Authorization %q with no token", gotAuth)
	}
}

func TestValidateBaseURL(t *testing.T) {
	for _, ok := range []string{
		"http://localhost:7070", "https://branchd.example", "http://10.0.0.5:7070/",
		"http://[::1]:7070", "https://gw.example/pgoverlay",
	} {
		if err := ValidateBaseURL(ok); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"localhost:7070", "branchd:7070", "ftp://branchd", "http://", "http:///v1", "/v1",
		"http://localhost:7070?x=1", "://nope",
	} {
		if err := ValidateBaseURL(bad); err == nil {
			t.Errorf("ValidateBaseURL(%q) = nil, want an error", bad)
		}
	}
}

// With PGOVERLAY_CA_CERT the client still uses the default transport's
// settings (proxy from the environment, timeouts, HTTP/2), only with the CA
// pool swapped in.
func TestCACertKeepsDefaultTransportSettings(t *testing.T) {
	t.Setenv("PGOVERLAY_CA_CERT", writeTestCACert(t))
	tr, ok := New("https://branchd.example:7070", "tok").HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("transport is not *http.Transport")
	}
	def := http.DefaultTransport.(*http.Transport)
	if tr.Proxy == nil || tr.TLSHandshakeTimeout != def.TLSHandshakeTimeout ||
		tr.IdleConnTimeout != def.IdleConnTimeout || tr.ForceAttemptHTTP2 != def.ForceAttemptHTTP2 || tr.DialContext == nil {
		t.Fatalf("transport lost the default settings: %+v", tr)
	}
	if tr == def {
		t.Fatal("client shares http.DefaultTransport; it must own a clone")
	}
}

// PGOVERLAY_CA_CERT is the secure option and wins over skip-verify: with both
// set, a server signed by another CA is still rejected, and the server whose
// certificate is in the file is accepted.
func TestCACertPreferredOverSkipVerify(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]api.Branch{{Name: "pr-1"}})
	}))
	t.Cleanup(ts.Close)

	t.Setenv("PGOVERLAY_TLS_SKIP_VERIFY", "1")
	t.Setenv("PGOVERLAY_CA_CERT", writeTestCACert(t)) // an unrelated CA
	c := New(ts.URL, "tok")
	if tr := c.HTTP.Transport.(*http.Transport); tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("skip-verify won over PGOVERLAY_CA_CERT")
	}
	if _, err := c.ListBranches(context.Background()); err == nil {
		t.Fatal("a server outside the CA file was accepted")
	}

	// the server's own certificate as the CA file: verified, accepted
	caFile := filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PGOVERLAY_CA_CERT", caFile)
	if _, err := New(ts.URL, "tok").ListBranches(context.Background()); err != nil {
		t.Fatalf("with the server's CA: %v", err)
	}
}
