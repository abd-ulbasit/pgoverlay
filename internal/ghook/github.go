package ghook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/api"
)

// GitHub is a minimal REST client for the one thing this service does on
// GitHub: a single connect-info comment per pull request. No SDK dependency.
type GitHub struct {
	BaseURL string // e.g. https://api.github.com (overridable for tests)
	Token   string
	HTTP    *http.Client
}

// commentMarker identifies the connect-info comment; its presence on a PR
// means we already commented (post once per PR).
const commentMarker = "<!-- pgbranch -->"

// EnsureComment posts body as an issue comment on repo#number unless a
// comment carrying the pgbranch marker already exists. Only the first page
// (100 comments) is checked — at worst a busy PR gets a duplicate comment.
func (g *GitHub) EnsureComment(ctx context.Context, repo string, number int, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)

	var existing []struct {
		Body string `json:"body"`
	}
	if err := g.do(ctx, "GET", path+"?per_page=100", nil, &existing); err != nil {
		return fmt.Errorf("list comments: %w", err)
	}
	for _, c := range existing {
		if strings.Contains(c.Body, commentMarker) {
			return nil // already commented on this PR
		}
	}
	if err := g.do(ctx, "POST", path, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("create comment: %w", err)
	}
	return nil
}

// do sends a JSON request to the GitHub REST API and decodes the response
// into out (skipped when out is nil).
func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := g.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("github: %s %s: HTTP %d: %.200s", method, path, resp.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// commentBody renders the connect-info comment for a branch. The marker
// makes the comment discoverable for dedup on later events.
func commentBody(proxyHost string, b *api.Branch) string {
	host, port := proxyHost, ""
	if h, p, err := net.SplitHostPort(proxyHost); err == nil {
		host, port = h, p
	}
	psql := fmt.Sprintf("psql -h %s", host)
	if port != "" {
		psql += " -p " + port
	}
	psql += fmt.Sprintf(" -U %s -d '%s'", b.User, b.ProxyDatabase)
	return fmt.Sprintf(`%s
**pgbranch** created the Postgres branch `+"`%s`"+` for this pull request.

Connect through the pgbranch proxy (the database name routes to the branch):

`+"```"+`
%s
`+"```"+`

The branch is destroyed when the pull request is closed.
`, commentMarker, b.Name, psql)
}
