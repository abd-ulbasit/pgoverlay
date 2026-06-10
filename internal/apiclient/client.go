// Package apiclient is a thin typed client for branchd's REST API, used by
// the CLI in server mode (PGBRANCH_SERVER / --server).
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/api"
)

type Client struct {
	BaseURL string // e.g. http://localhost:7070 or https://branchd.example:7070
	Token   string // bearer token (PGBRANCH_TOKEN)
	HTTP    *http.Client
}

// New builds a client for the given base URL (http or https). For branchd
// instances serving a self-signed certificate, PGBRANCH_TLS_SKIP_VERIFY=1
// disables certificate verification (read once, here).
func New(baseURL, token string) *Client {
	httpClient := http.DefaultClient
	if os.Getenv("PGBRANCH_TLS_SKIP_VERIFY") == "1" {
		httpClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: httpClient}
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
