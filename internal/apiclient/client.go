// Package apiclient is a thin typed client for branchd's REST API, used by
// the CLI in server mode (PGBRANCH_SERVER / --server).
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/api"
)

type Client struct {
	BaseURL string // e.g. http://localhost:7070
	Token   string // bearer token (PGBRANCH_TOKEN)
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: http.DefaultClient}
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
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
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
