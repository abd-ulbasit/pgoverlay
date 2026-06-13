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
	"github.com/abd-ulbasit/pgbranch/internal/engine"
)

// TokenProvider supplies the bearer token for a GitHub API call. PAT mode is
// a constant provider (StaticToken); App mode mints per-installation tokens
// (AppAuth.Token) keyed by the webhook delivery's installation id.
type TokenProvider func(ctx context.Context, installationID int64) (string, error)

// StaticToken is the PAT-mode TokenProvider: always the same token,
// whatever the installation.
func StaticToken(token string) TokenProvider {
	return func(context.Context, int64) (string, error) { return token, nil }
}

// GitHub is a minimal REST client for what this service does on GitHub: the
// live connect-info comment and the pgbranch/branch commit status. No SDK
// dependency.
type GitHub struct {
	BaseURL string // e.g. https://api.github.com (overridable for tests)
	Token   TokenProvider
	HTTP    *http.Client

	// installationID scopes App-mode tokens to the delivery being handled;
	// bound per delivery via ForInstallation, ignored by StaticToken.
	installationID int64
}

// ForInstallation returns a shallow copy of the client bound to the given
// installation id (from the webhook payload). Safe for the PAT provider,
// which ignores it.
func (g *GitHub) ForInstallation(id int64) *GitHub {
	c := *g
	c.installationID = id
	return &c
}

// commentMarker identifies the live connect-info comment; the upsert finds
// it on later events and rewrites it in place (one comment per PR, always
// current). diffMarker identifies the separate schema/data diff comment, so
// the two are upserted independently and never clobber each other.
const (
	commentMarker = "<!-- pgbranch -->"
	diffMarker    = "<!-- pgbranch-diff -->"
)

// statusContext is the commit-status context CI can gate on.
const statusContext = "pgbranch/branch"

// SetStatus sets the pgbranch/branch commit status on a commit: pending
// while a branch operation runs, then success or failure. CI consumers gate
// on the context instead of polling the branch with psql retry loops.
// Descriptions are truncated to GitHub's 140-character limit.
func (g *GitHub) SetStatus(ctx context.Context, repo, sha, state, desc string) error {
	body := map[string]string{
		"state":       state,
		"description": truncate(desc, 140),
		"context":     statusContext,
	}
	path := fmt.Sprintf("/repos/%s/statuses/%s", repo, sha)
	if err := g.do(ctx, "POST", path, body, nil); err != nil {
		return fmt.Errorf("set status: %w", err)
	}
	return nil
}

// truncate caps s at n runes (GitHub counts characters, not bytes).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// UpsertComment makes the comment carrying marker on repo#number carry body:
// PATCH in place when it exists, POST otherwise. Each marker is upserted
// independently (e.g. the connect comment vs the diff comment).
func (g *GitHub) UpsertComment(ctx context.Context, repo string, number int, marker, body string) error {
	id, err := g.findMarkerComment(ctx, repo, number, marker)
	if err != nil {
		return err
	}
	if id != 0 {
		return g.patchComment(ctx, repo, id, body)
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number)
	if err := g.do(ctx, "POST", path, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("create comment: %w", err)
	}
	return nil
}

// UpdateComment rewrites the marker comment when present and does nothing
// when it isn't — closing a PR that never got a comment shouldn't create
// one just to say the branch is gone.
func (g *GitHub) UpdateComment(ctx context.Context, repo string, number int, marker, body string) error {
	id, err := g.findMarkerComment(ctx, repo, number, marker)
	if err != nil || id == 0 {
		return err
	}
	return g.patchComment(ctx, repo, id, body)
}

// findMarkerComment returns the id of the comment carrying marker, or 0 when
// there is none. Only the first page (100 comments) is checked — at worst a
// busy PR gets a duplicate comment.
func (g *GitHub) findMarkerComment(ctx context.Context, repo string, number int, marker string) (int64, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo, number)
	var existing []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := g.do(ctx, "GET", path, nil, &existing); err != nil {
		return 0, fmt.Errorf("list comments: %w", err)
	}
	for _, c := range existing {
		if strings.Contains(c.Body, marker) {
			return c.ID, nil
		}
	}
	return 0, nil
}

func (g *GitHub) patchComment(ctx context.Context, repo string, id int64, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id)
	if err := g.do(ctx, "PATCH", path, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("update comment: %w", err)
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
	token, err := g.Token(ctx, g.installationID)
	if err != nil {
		return fmt.Errorf("github token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
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

// commentBody renders the live status comment for a branch: a small table
// with the branch name, its state (creating/resetting/ready/reset @ sha/
// destroyed), the psql connect string and the expiry when a TTL is set.
// The marker makes the comment discoverable for the in-place update on
// later events. b is nil while the branch operation is still running (and
// after destroy), so connect info is omitted then.
func commentBody(proxyHost, branch, state string, b *api.Branch) string {
	var sb strings.Builder
	sb.WriteString(commentMarker + "\n")
	sb.WriteString("**pgbranch** — Postgres branch for this pull request.\n\n")
	sb.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&sb, "| Branch | `%s` |\n", branch)
	fmt.Fprintf(&sb, "| State | %s |\n", state)
	if b != nil {
		fmt.Fprintf(&sb, "| Connect | `%s` |\n", psqlCommand(proxyHost, b))
		if b.ExpiresAt != "" {
			fmt.Fprintf(&sb, "| Expires | %s |\n", b.ExpiresAt)
		}
	}
	if state == "destroyed" {
		sb.WriteString("\nThe branch was destroyed when the pull request closed.\n")
	} else {
		sb.WriteString("\nThe database name routes through the pgbranch proxy to the branch. " +
			"The branch is destroyed when the pull request is closed.\n")
	}
	return sb.String()
}

// diffSchemaLimit caps the schema diff embedded in the PR comment; longer
// diffs are truncated with a note so the comment stays readable and within
// GitHub's body size limits.
const diffSchemaLimit = 3000

// diffCommentBody renders the schema/data diff comment for a branch: the
// schema diff inside a ```diff fence (truncated to diffSchemaLimit chars with
// a note), followed by the per-table row-estimate delta table. Carries the
// diff marker so it is upserted independently of the connect comment.
func diffCommentBody(branch string, res *engine.DiffResult) string {
	var sb strings.Builder
	sb.WriteString(diffMarker + "\n")
	fmt.Fprintf(&sb, "**pgbranch diff** — what changed in `%s` vs its base.\n\n", branch)

	if res.SchemaDiff == "" {
		sb.WriteString("Schema: no differences.\n")
	} else {
		schema := res.SchemaDiff
		truncated := false
		if len([]rune(schema)) > diffSchemaLimit {
			schema = string([]rune(schema)[:diffSchemaLimit])
			truncated = true
		}
		sb.WriteString("Schema diff:\n\n```diff\n")
		sb.WriteString(schema)
		if !strings.HasSuffix(schema, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
		if truncated {
			fmt.Fprintf(&sb, "\n_(schema diff truncated to %d characters)_\n", diffSchemaLimit)
		}
	}

	var changed []engine.TableDelta
	for _, t := range res.Tables {
		if t.Delta != 0 {
			changed = append(changed, t)
		}
	}
	if len(changed) == 0 {
		sb.WriteString("\nTables: no row-count changes.\n")
		return sb.String()
	}
	sb.WriteString("\n| TABLE | BASE | BRANCH | DELTA |\n|---|---|---|---|\n")
	for _, t := range changed {
		fmt.Fprintf(&sb, "| `%s` | %d | %d | %+d |\n", t.Table, t.BaseRows, t.BranchRows, t.Delta)
	}
	sb.WriteString("\n_(row counts are planner estimates)_\n")
	return sb.String()
}

// psqlCommand renders the proxy connect string shown in comments.
func psqlCommand(proxyHost string, b *api.Branch) string {
	host, port := proxyHost, ""
	if h, p, err := net.SplitHostPort(proxyHost); err == nil {
		host, port = h, p
	}
	psql := fmt.Sprintf("psql -h %s", host)
	if port != "" {
		psql += " -p " + port
	}
	return psql + fmt.Sprintf(" -U %s -d '%s'", b.User, b.ProxyDatabase)
}
