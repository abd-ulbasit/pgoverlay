// Package apiclient is a thin typed client for branchd's REST API, used by
// the CLI in server mode (PGOVERLAY_SERVER / --server).
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
)

type Client struct {
	BaseURL string // e.g. http://localhost:7070 or https://branchd.example:7070
	Token   string // bearer token (PGOVERLAY_TOKEN)
	HTTP    *http.Client
	// Retry bounds how requests that failed in a repeat-safe way are retried
	// (see RetryPolicy). New sets DefaultRetry; the zero value disables
	// retries.
	Retry RetryPolicy
}

// RetryPolicy bounds the retries of one request. A request is retried only
// when repeating it is safe:
//   - 503 Service Unavailable, for any method. branchd answers 503 from its
//     HA leader gate ("not leader") and while shutting down, before doing
//     any work, and for a mutation interrupted by a leadership change or
//     shutdown after its saga rolled the partial work back; proxies answer
//     503 when no backend took the request.
//   - 502 and 504 from a proxy in front of branchd, for idempotent methods.
//     branchd's own 504 (an operation that ran into the stuck timeout and
//     was rolled back) is not retried: repeating it would run the whole
//     operation again, for as long again.
//   - A connection that could not be established (nothing was sent), for any
//     method, except a host name that does not resolve.
//   - A connection reset or closed mid-request, for idempotent methods.
//
// Waits grow exponentially from BaseDelay, are capped at MaxDelay and
// jittered; a Retry-After header on a 503 is honoured up to MaxDelay. Before
// each retry the client drops its idle connections, so a request refused by a
// follower replica is re-dialled and a Service can route it elsewhere.
type RetryPolicy struct {
	MaxAttempts int           // total tries including the first; <= 1 disables retries
	BaseDelay   time.Duration // first wait
	MaxDelay    time.Duration // cap on any single wait
}

// DefaultRetry tries a request up to 6 times over at most about 8 seconds of
// waiting: long enough to ride out a follower replica or a branchd restart,
// short enough that an interactive command still fails promptly.
var DefaultRetry = RetryPolicy{MaxAttempts: 6, BaseDelay: 250 * time.Millisecond, MaxDelay: 4 * time.Second}

// ValidateBaseURL checks that s is an absolute http:// or https:// URL with a
// host, the form New expects. A bare "localhost:7070" parses as a URL with
// scheme "localhost", so without this check it fails later inside net/http
// with "unsupported protocol scheme".
func ValidateBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", s, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("%q must be an http:// or https:// URL with a host, e.g. http://localhost:7070", s)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must not carry a query or fragment", s)
	}
	return nil
}

// New builds a client for the given base URL (http or https). It uses a clone
// of http.DefaultTransport (proxy settings from the environment, dial and TLS
// handshake timeouts, HTTP/2), adjusted for TLS in order of preference:
//   - PGOVERLAY_CA_CERT=<pem-file> trusts a self-signed or private-CA branchd
//     properly by verifying against the PEM's certificates. When it loads,
//     PGOVERLAY_TLS_SKIP_VERIFY is ignored.
//   - PGOVERLAY_TLS_SKIP_VERIFY=1 disables certificate verification entirely;
//     supported as an escape hatch but warned about loudly (MITM-exposed).
//
// Either way proxy settings from the environment (HTTPS_PROXY/NO_PROXY), the
// dial and TLS handshake timeouts and keep-alives still apply.
//
// It also warns once, to stderr, when the token would be sent over plaintext
// http to a non-loopback host (cleartext bearer token on the wire). It does
// not hard-fail: some deployments front branchd with a trusted TLS proxy.
func New(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	tr := defaultTransport()

	caLoaded := false
	caPath := os.Getenv("PGOVERLAY_CA_CERT")
	if caPath != "" {
		if tlsCfg, err := tlsConfigWithCA(caPath); err != nil {
			warnf("pgoverlay: PGOVERLAY_CA_CERT %q could not be loaded (%v); falling back to system roots", caPath, err)
		} else {
			tr.TLSClientConfig = tlsCfg
			caLoaded = true
		}
	}
	if os.Getenv("PGOVERLAY_TLS_SKIP_VERIFY") == "1" {
		if caLoaded {
			warnf("pgoverlay: PGOVERLAY_TLS_SKIP_VERIFY=1 is ignored: PGOVERLAY_CA_CERT is set, so certificates are verified against %s", caPath)
		} else {
			warnf("pgoverlay: PGOVERLAY_TLS_SKIP_VERIFY=1 disables TLS certificate verification — the connection is exposed to man-in-the-middle attacks; prefer PGOVERLAY_CA_CERT=<pem file>")
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
	}
	if token != "" && plaintextTokenLeak(baseURL) {
		warnf("pgoverlay: sending bearer token in cleartext over http to a non-loopback host (%s) — use https or PGOVERLAY_CA_CERT", baseURL)
	}

	return &Client{BaseURL: baseURL, Token: token, HTTP: &http.Client{Transport: tr}, Retry: DefaultRetry}
}

// defaultTransport clones http.DefaultTransport so the client keeps its proxy,
// timeout and HTTP/2 settings, and owns its connection pool (retries close
// idle connections without touching other users of the default transport).
func defaultTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
}

// warnf prints a one-line warning to stderr. Kept tiny and dependency-free so
// the CLI surfaces transport-safety issues without pulling in a logger.
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// plaintextTokenLeak reports whether sending a bearer token to rawURL would
// expose it in cleartext: the scheme is http (not https) AND the host is not
// loopback. Loopback http is fine (the token never leaves the machine);
// remote http leaks it on the wire. Unparseable/relative/other-scheme URLs
// return false — we only warn on a clear, actionable cleartext-to-remote case.
func plaintextTokenLeak(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return !isLoopbackHost(u.Hostname())
}

// isLoopbackHost reports whether host is the loopback interface by name or IP
// (localhost, 127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// tlsConfigWithCA loads a PEM bundle from path into a fresh root pool and
// returns a tls.Config that trusts exactly those roots (so a self-signed
// branchd verifies properly, without disabling verification).
func tlsConfigWithCA(path string) (*tls.Config, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificates found in %s", path)
	}
	return &tls.Config{RootCAs: pool}, nil
}

// StatusError is returned for non-2xx responses; it carries the HTTP status
// so callers can branch on it (e.g. tolerate 404s).
type StatusError struct {
	StatusCode int
	Message    string
	// fromBranchd is set when the body was branchd's JSON error: the status
	// came from branchd itself, not from a proxy in front of it.
	fromBranchd bool
}

func (e *StatusError) Error() string { return e.Message }

// IsNotFound reports whether err is a server response with status 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusNotFound
}

// do sends a JSON request and decodes the JSON response into out (skipped if
// out is nil). Non-2xx responses become errors carrying the server's message.
// Failures that are safe to repeat are retried per c.Retry.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		payload = b
	}
	attempts := max(c.Retry.MaxAttempts, 1)
	for attempt := 1; ; attempt++ {
		data, retryAfter, err := c.once(ctx, method, path, payload, in != nil)
		if err == nil {
			if out == nil {
				return nil
			}
			return json.Unmarshal(data, out)
		}
		if attempt >= attempts || !retryable(method, err) {
			if attempt > 1 {
				err = gaveUp(err, attempt)
			}
			return err
		}
		// Drop pooled keep-alive connections so the retry dials afresh: a
		// Service balances per connection, so reusing the one that reached a
		// follower replica would reach it again.
		c.HTTP.CloseIdleConnections()
		wait := c.Retry.backoff(attempt, retryAfter)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (while retrying after: %v)", ctx.Err(), err)
		case <-t.C:
		}
	}
}

// once performs a single request. It returns the response body on 2xx, or an
// error plus the server's Retry-After (0 when absent).
func (c *Client) once(ctx context.Context, method, path string, payload []byte, hasBody bool) ([]byte, time.Duration, error) {
	var body io.Reader
	if hasBody {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, 0, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return data, 0, nil
	}
	var e struct {
		Error string `json:"error"`
	}
	msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
	fromBranchd := json.Unmarshal(data, &e) == nil && e.Error != ""
	if fromBranchd {
		msg = e.Error
	}
	if resp.StatusCode == http.StatusUnauthorized && c.Token == "" {
		msg += " (no bearer token was sent)"
	}
	return nil, retryAfterHeader(resp.Header.Get("Retry-After")),
		&StatusError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("%s %s: %s", method, path, msg), fromBranchd: fromBranchd}
}

// retryable reports whether a failed request may be sent again (see
// RetryPolicy for the rules).
func retryable(method string, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch se.StatusCode {
		case http.StatusServiceUnavailable:
			return true
		case http.StatusBadGateway, http.StatusGatewayTimeout:
			return idempotent(method) && !se.fromBranchd
		}
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		var dns *net.DNSError
		if errors.As(err, &dns) && dns.IsNotFound {
			return false // a typo in the host name does not fix itself
		}
		return true // the connection was never made, so nothing was sent
	}
	if !idempotent(method) {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return false
}

// backoff is the wait before retry number attempt (1-based): exponential from
// BaseDelay, capped at MaxDelay, jittered into [d/2, d]; a server Retry-After
// raises it, still capped at MaxDelay.
func (p RetryPolicy) backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := p.BaseDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		d = p.MaxDelay
	}
	if d > 1 {
		d = d/2 + rand.N(d/2+1)
	}
	if retryAfter > d {
		d = retryAfter
		if p.MaxDelay > 0 && d > p.MaxDelay {
			d = p.MaxDelay
		}
	}
	return d
}

// retryAfterHeader parses a Retry-After value in seconds (the HTTP-date form
// is not used by branchd or common proxies and is ignored).
func retryAfterHeader(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// gaveUp notes the attempt count on the final error, keeping its type so
// callers can still inspect a *StatusError.
func gaveUp(err error, attempts int) error {
	var se *StatusError
	if errors.As(err, &se) {
		return &StatusError{StatusCode: se.StatusCode, Message: fmt.Sprintf("%s (gave up after %d attempts)", se.Message, attempts)}
	}
	return fmt.Errorf("%w (gave up after %d attempts)", err, attempts)
}

func (c *Client) CreateSource(ctx context.Context, req api.CreateSourceRequest) (*api.Source, error) {
	var s api.Source
	if err := c.do(ctx, "POST", "/v1/sources", req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) ListSources(ctx context.Context) ([]api.Source, error) {
	var out []api.Source
	return out, c.do(ctx, "GET", "/v1/sources", nil, &out)
}

func (c *Client) RemoveSource(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1/sources/"+url.PathEscape(name), nil, nil)
}

func (c *Client) RefreshSource(ctx context.Context, name, password string) (*api.Source, error) {
	var s api.Source
	if err := c.do(ctx, "POST", "/v1/sources/"+url.PathEscape(name)+"/refresh",
		api.RefreshSourceRequest{Password: password}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SetMaskScripts replaces a source's masking scripts (empty slice clears
// them) and returns the stored list.
func (c *Client) SetMaskScripts(ctx context.Context, name string, scripts []api.MaskScript) ([]api.MaskScript, error) {
	if scripts == nil {
		scripts = []api.MaskScript{}
	}
	var out []api.MaskScript
	return out, c.do(ctx, "PUT", "/v1/sources/"+url.PathEscape(name)+"/mask", scripts, &out)
}

func (c *Client) GetMaskScripts(ctx context.Context, name string) ([]api.MaskScript, error) {
	var out []api.MaskScript
	return out, c.do(ctx, "GET", "/v1/sources/"+url.PathEscape(name)+"/mask", nil, &out)
}

func (c *Client) CreateBranch(ctx context.Context, req api.CreateBranchRequest) (*api.Branch, error) {
	var b api.Branch
	if err := c.do(ctx, "POST", "/v1/branches", req, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (c *Client) ListBranches(ctx context.Context) ([]api.Branch, error) {
	var out []api.Branch
	return out, c.do(ctx, "GET", "/v1/branches", nil, &out)
}

func (c *Client) GetBranch(ctx context.Context, name string) (*api.Branch, error) {
	var b api.Branch
	if err := c.do(ctx, "GET", "/v1/branches/"+url.PathEscape(name), nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BranchUsage returns the branch's rw-layer disk usage in bytes. The server
// runs a helper container per call — treat it as an on-demand probe.
func (c *Client) BranchUsage(ctx context.Context, name string) (int64, error) {
	var out struct {
		Bytes int64 `json:"bytes"`
	}
	if err := c.do(ctx, "GET", "/v1/branches/"+url.PathEscape(name)+"/usage", nil, &out); err != nil {
		return 0, err
	}
	return out.Bytes, nil
}

// DiffBranch returns what changed in a branch relative to its base (unified
// schema diff + per-table row-estimate deltas). The server provisions a
// throwaway clone of the branch's base and pg_dumps both instances per call —
// expect ~5-10s. dataSample, when > 0, asks the server for up to that many
// branch-only sample rows per grown table (?data=N); 0 disables sampling.
func (c *Client) DiffBranch(ctx context.Context, name string, dataSample int) (*engine.DiffResult, error) {
	path := "/v1/branches/" + url.PathEscape(name) + "/diff"
	if dataSample > 0 {
		path += "?data=" + strconv.Itoa(dataSample)
	}
	var out engine.DiffResult
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BranchHistory fetches a branch's audit trail: every recorded state
// transition with its reason, the actor that caused it, and the timestamp,
// oldest first. Backs `pgb history` in server mode.
func (c *Client) BranchHistory(ctx context.Context, name string) ([]api.Transition, error) {
	var out []api.Transition
	return out, c.do(ctx, "GET", "/v1/branches/"+url.PathEscape(name)+"/history", nil, &out)
}

func (c *Client) DestroyBranch(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1/branches/"+url.PathEscape(name), nil, nil)
}

func (c *Client) ResetBranch(ctx context.Context, name string) (*api.Branch, error) {
	var b api.Branch
	if err := c.do(ctx, "POST", "/v1/branches/"+url.PathEscape(name)+"/reset", nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// RecoverBranch restarts a failed branch on its existing data. Backs
// `pgb branch recover` in server mode.
func (c *Client) RecoverBranch(ctx context.Context, name string) (*api.Branch, error) {
	var b api.Branch
	if err := c.do(ctx, "POST", "/v1/branches/"+url.PathEscape(name)+"/recover", nil, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ReconcilePlan fetches the read-only convergence plan (drift report) from the
// server. Backs `pgb doctor` in server mode.
func (c *Client) ReconcilePlan(ctx context.Context) (*engine.ReconcilePlan, error) {
	var p engine.ReconcilePlan
	if err := c.do(ctx, "GET", "/v1/reconcile/plan", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ReconcileApply runs a reconcile pass on the server and returns the actions
// taken. Backs `pgb gc` in server mode.
func (c *Client) ReconcileApply(ctx context.Context) (*engine.ReconcilePlan, error) {
	var p engine.ReconcilePlan
	if err := c.do(ctx, "POST", "/v1/reconcile", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreateToken mints an API token of the given role and returns its plaintext
// value (shown once). Admin-only on the server.
func (c *Client) CreateToken(ctx context.Context, name, role string) (string, error) {
	var resp api.CreateTokenResponse
	if err := c.do(ctx, "POST", "/v1/tokens", api.CreateTokenRequest{Name: name, Role: role}, &resp); err != nil {
		return "", err
	}
	return resp.Token, nil
}

// ListTokens returns token metadata (never the plaintext). Admin-only.
func (c *Client) ListTokens(ctx context.Context) ([]api.Token, error) {
	var out []api.Token
	return out, c.do(ctx, "GET", "/v1/tokens", nil, &out)
}

// RevokeToken deletes a token by name. Admin-only.
func (c *Client) RevokeToken(ctx context.Context, name string) error {
	return c.do(ctx, "DELETE", "/v1/tokens/"+url.PathEscape(name), nil, nil)
}
