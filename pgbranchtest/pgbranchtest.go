// Package pgbranchtest gives every Go test its own disposable Postgres
// database: Acquire creates a copy-on-write branch on a running pgbranch
// server (branchd), waits until it is ready, and destroys it when the test
// finishes.
//
//	func TestOrders(t *testing.T) {
//		b := pgbranchtest.Acquire(t)
//		db, _ := sql.Open("pgx", b.DSN)
//		// full production-shaped data, isolated writes
//	}
//
// Configuration comes from the environment: PGBRANCH_SERVER (base URL of
// branchd; tests are skipped when unset — the SDK is integration-only by
// nature), PGBRANCH_TOKEN (API bearer token), PGBRANCH_TEST_SOURCE (default
// source name, else "main"), and PGBRANCH_PASSWORD (database password used in
// the returned DSNs; branch credentials are inherited from the source unless
// the server rotates them per branch and returns one).
//
// The package is intentionally self-contained (stdlib only): it speaks the
// branchd REST API directly and never imports pgbranch internals.
package pgbranchtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Branch is an acquired database branch. DSN targets the branch's Postgres
// directly; ProxyDSN goes through the pgbranch wire-protocol router on the
// server host (port 6432, database "db@branch").
type Branch struct {
	Name     string
	Host     string
	Port     int
	User     string
	Password string
	Database string
	DSN      string
	ProxyDSN string
}

type config struct {
	source string
	ttl    time.Duration
}

// Option customizes Acquire.
type Option func(*config)

// WithSource selects the source to branch from. Default: PGBRANCH_TEST_SOURCE,
// else "main".
func WithSource(name string) Option { return func(c *config) { c.source = name } }

// WithTTL sets the branch TTL — a server-side safety net in case the process
// dies before t.Cleanup runs. Default 1h; explicit destroy on test end is the
// primary cleanup.
func WithTTL(d time.Duration) Option { return func(c *config) { c.ttl = d } }

// pollInterval is how often Acquire re-checks a not-yet-ready branch
// (variable so unit tests can speed it up).
var pollInterval = time.Second

// acquireTimeout bounds create + ready-wait.
const acquireTimeout = 5 * time.Minute

// Acquire creates a branch named t-<test name>-<random> and registers its
// destruction with t.Cleanup. It is safe for parallel tests: every call gets
// its own branch. The test is skipped when PGBRANCH_SERVER is unset.
func Acquire(t testing.TB, opts ...Option) *Branch {
	t.Helper()
	server := os.Getenv("PGBRANCH_SERVER")
	if server == "" {
		t.Skip("pgbranchtest: PGBRANCH_SERVER not set, skipping")
	}
	cfg := config{source: os.Getenv("PGBRANCH_TEST_SOURCE"), ttl: time.Hour}
	if cfg.source == "" {
		cfg.source = "main"
	}
	for _, o := range opts {
		o(&cfg)
	}

	c := &client{base: strings.TrimRight(server, "/"), token: os.Getenv("PGBRANCH_TOKEN")}
	name := branchName(t.Name(), randHex(6))

	ctx, cancel := context.WithTimeout(context.Background(), acquireTimeout)
	defer cancel()

	b, err := c.createBranch(ctx, createBranchRequest{
		Name: name, Source: cfg.source, TTLSeconds: int(cfg.ttl / time.Second),
	})
	if err != nil {
		t.Fatalf("pgbranchtest: create branch %q from %q: %v", name, cfg.source, err)
	}
	// register destruction first so a failed ready-wait still cleans up
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := c.destroyBranch(ctx, name); err != nil {
			t.Errorf("pgbranchtest: destroy branch %q: %v", name, err)
		}
	})

	// The create endpoint returns ready synchronously today, but don't depend
	// on it: poll GET until the branch reports ready.
	for b.State != "ready" {
		select {
		case <-ctx.Done():
			t.Fatalf("pgbranchtest: branch %q not ready after %s (state %q)", name, acquireTimeout, b.State)
		case <-time.After(pollInterval):
		}
		b, err = c.getBranch(ctx, name)
		if err != nil {
			t.Fatalf("pgbranchtest: wait for branch %q: %v", name, err)
		}
	}
	return newBranch(b, c.base)
}

// newBranch maps the wire shape to the public Branch, building DSNs. The
// direct host falls back to the server's hostname for older servers that
// don't send one; the password prefers a server-returned per-branch secret
// (credential-rotation mode) over the PGBRANCH_PASSWORD fallback.
func newBranch(w *wireBranch, baseURL string) *Branch {
	serverHost := ""
	if u, err := url.Parse(baseURL); err == nil {
		serverHost = u.Hostname()
	}
	b := &Branch{
		Name: w.Name, Host: w.Host, Port: w.Port,
		User: w.User, Password: w.Password, Database: w.Database,
	}
	if b.Host == "" {
		b.Host = serverHost
	}
	if b.User == "" {
		b.User = "postgres"
	}
	if b.Database == "" {
		b.Database = "postgres"
	}
	if b.Password == "" {
		b.Password = os.Getenv("PGBRANCH_PASSWORD")
	}
	b.DSN = dsn(b.User, b.Password, b.Host, b.Port, b.Database)
	b.ProxyDSN = dsn(b.User, b.Password, serverHost, 6432, w.ProxyDatabase)
	return b
}

// dsn builds postgres://user[:password]@host:port/db. The db may contain '@'
// (proxy routing) — legal in a URL path, kept literal.
func dsn(user, password, host string, port int, db string) string {
	auth := url.QueryEscape(user)
	if password != "" {
		auth += ":" + url.QueryEscape(password)
	}
	return fmt.Sprintf("postgres://%s@%s/%s", auth, net.JoinHostPort(host, strconv.Itoa(port)), db)
}

// branchName builds t-<sanitized test name>-<suffix>, ≤41 chars (the server's
// ^[a-z0-9][a-z0-9-]{0,40}$ rule). Sanitizing matches ghook: lowercase, runs
// of other characters collapse to single dashes. Long test names are
// truncated from the LEFT so the most specific part (the subtest) survives
// alongside the random suffix.
func branchName(testName, suffix string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(testName) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dash && sb.Len() > 0 {
				sb.WriteByte('-')
			}
			dash = false
			sb.WriteRune(r)
		default:
			dash = true
		}
	}
	s := sb.String()
	if max := 41 - len("t-") - 1 - len(suffix); len(s) > max {
		s = strings.TrimLeft(s[len(s)-max:], "-")
	}
	s = strings.Trim(s, "-")
	if s == "" {
		return "t-" + suffix
	}
	return "t-" + s + "-" + suffix
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic("pgbranchtest: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}

// --- minimal branchd REST client (wire types mirror internal/api) ---

// wireBranch mirrors the JSON shape of branchd's Branch responses. password
// is only sent by servers running per-branch credential rotation.
type wireBranch struct {
	Name          string `json:"name"`
	Source        string `json:"source"`
	State         string `json:"state"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	Database      string `json:"database"`
	ProxyDatabase string `json:"proxy_database"`
	ExpiresAt     string `json:"expires_at"`
	CreatedAt     string `json:"created_at"`
}

// createBranchRequest mirrors POST /v1/branches.
type createBranchRequest struct {
	Name       string `json:"name"`
	Source     string `json:"source,omitempty"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type client struct {
	base  string
	token string
}

func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, data, nil
}

func (c *client) createBranch(ctx context.Context, req createBranchRequest) (*wireBranch, error) {
	code, data, err := c.do(ctx, http.MethodPost, "/v1/branches", req)
	if err != nil {
		return nil, err
	}
	if code != http.StatusCreated {
		return nil, fmt.Errorf("POST /v1/branches: HTTP %d: %s", code, strings.TrimSpace(string(data)))
	}
	var b wireBranch
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("POST /v1/branches: decode response: %w", err)
	}
	return &b, nil
}

func (c *client) getBranch(ctx context.Context, name string) (*wireBranch, error) {
	code, data, err := c.do(ctx, http.MethodGet, "/v1/branches/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/branches/%s: HTTP %d: %s", name, code, strings.TrimSpace(string(data)))
	}
	var b wireBranch
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("GET /v1/branches/%s: decode response: %w", name, err)
	}
	return &b, nil
}

// destroyBranch deletes the branch; a 404 means it is already gone (TTL
// reaper or explicit delete) and is not an error.
func (c *client) destroyBranch(ctx context.Context, name string) error {
	code, data, err := c.do(ctx, http.MethodDelete, "/v1/branches/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	if code != http.StatusNoContent && code != http.StatusNotFound {
		return fmt.Errorf("DELETE /v1/branches/%s: HTTP %d: %s", name, code, strings.TrimSpace(string(data)))
	}
	return nil
}
