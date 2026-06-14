// Package apiclient is a thin typed client for branchd's REST API, used by
// the CLI in server mode (PGBRANCH_SERVER / --server).
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
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

type Client struct {
	BaseURL string // e.g. http://localhost:7070 or https://branchd.example:7070
	Token   string // bearer token (PGBRANCH_TOKEN)
	HTTP    *http.Client
}

// New builds a client for the given base URL (http or https). Transport
// safety, in order of preference:
//   - PGBRANCH_CA_CERT=<pem-file> trusts a self-signed branchd properly by
//     adding the PEM to the root pool (the right way to use a private CA).
//   - PGBRANCH_TLS_SKIP_VERIFY=1 disables certificate verification entirely;
//     supported as an escape hatch but warned about loudly (MITM-exposed).
//
// It also warns once, to stderr, when the token would be sent over plaintext
// http to a non-loopback host (cleartext bearer token on the wire). It does
// not hard-fail: some deployments front branchd with a trusted TLS proxy.
func New(baseURL, token string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	httpClient := http.DefaultClient

	if caPath := os.Getenv("PGBRANCH_CA_CERT"); caPath != "" {
		if tlsCfg, err := tlsConfigWithCA(caPath); err != nil {
			warnf("pgbranch: PGBRANCH_CA_CERT %q could not be loaded (%v); falling back to system roots", caPath, err)
		} else {
			httpClient = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
		}
	}
	if os.Getenv("PGBRANCH_TLS_SKIP_VERIFY") == "1" {
		warnf("pgbranch: PGBRANCH_TLS_SKIP_VERIFY=1 disables TLS certificate verification — the connection is exposed to man-in-the-middle attacks")
		httpClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}
	if token != "" && plaintextTokenLeak(baseURL) {
		warnf("pgbranch: sending bearer token in cleartext over http to a non-loopback host (%s) — use https or PGBRANCH_CA_CERT", baseURL)
	}

	return &Client{BaseURL: baseURL, Token: token, HTTP: httpClient}
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
}

func (e *StatusError) Error() string { return e.Message }

// IsNotFound reports whether err is a server response with status 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusNotFound
}

// do sends a JSON request and decodes the JSON response into out (skipped if
// out is nil). Non-2xx responses become errors carrying the server's message.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error string `json:"error"`
		}
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return &StatusError{StatusCode: resp.StatusCode, Message: fmt.Sprintf("%s %s: %s", method, path, msg)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
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
