// Package pgoverlayconnect resolves a ready Postgres connection string for a
// pgoverlay branch by asking branchd for the branch's current credentials.
//
// It exists to reconcile two pgoverlay features that are otherwise in tension:
// per-branch credential rotation (each branch has its own password) and
// static application configuration (a 12-factor app holds fixed env vars, not
// a per-branch password). With this helper the static config an app holds is
// the branchd API endpoint plus a scoped (viewer) token; the per-branch
// password is fetched at startup:
//
//	res, err := pgoverlayconnect.Resolve(ctx, pgoverlayconnect.Options{
//		Server: os.Getenv("PGOVERLAY_API"),     // https://branchd:7070
//		Token:  os.Getenv("PGOVERLAY_TOKEN"),   // a viewer token is enough
//		Repo:   os.Getenv("GITHUB_REPOSITORY"), // "acme/widgets"
//		PR:     prNumber,                       // 7 -> gh-<key>-pr-7
//	})
//	db, _ := sql.Open("pgx", res.ProxyDSN)
//
// Repo with PR (or with Ref, for git-branch naming) derives the name that
// pgoverlay-github, the GitHub App webhook service, gives the pull request's
// branch; see PRBranchName and RefBranchName. The package is self-contained
// (stdlib only) and never imports pgoverlay internals: it speaks the branchd
// REST API directly.
package pgoverlayconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Options configures Resolve. Branch names the branch exactly. Without it,
// the name is derived the way pgoverlay-github names pull-request branches,
// so an app and the webhook agree on it with no coordination: Repo plus PR
// gives the pr-number name (the service default), Repo plus Ref the
// git-branch name. With both Ref and PR set, Ref is used unless it has no
// ASCII letter or digit, the same fallback the service applies. Pull requests
// from forks are always named by number.
type Options struct {
	Server string // branchd base URL (PGOVERLAY_API), required
	Token  string // API bearer token (a viewer token suffices), required
	Branch string // exact branch name; or set Repo with PR and/or Ref
	// Repo is the GitHub repository the pull request belongs to, "owner/name"
	// (GITHUB_REPOSITORY in Actions; VERCEL_GIT_REPO_OWNER + "/" +
	// VERCEL_GIT_REPO_SLUG on Vercel). Required with PR or Ref.
	Repo string
	PR   int // pull request number (pr-number naming)
	// Ref is the pull request's head branch, e.g. "feat/login"
	// (GITHUB_HEAD_REF, VERCEL_GIT_COMMIT_REF), for git-branch naming. Not
	// a full ref such as "refs/heads/feat/login".
	Ref       string
	ProxyHost string // host[:port] of the pgoverlay router for ProxyDSN; "" => server host + :6432
	Password  string // fallback password for inherit-mode branches (else PGPASSWORD)
	HTTP      *http.Client
}

// branchName returns the branch Resolve looks up (see Options).
func (opts Options) branchName() (string, error) {
	if opts.Branch != "" {
		return opts.Branch, nil
	}
	if opts.Ref == "" && opts.PR <= 0 {
		return "", fmt.Errorf("pgoverlayconnect: a Branch, or a Repo with a PR or Ref, is required")
	}
	if opts.Repo == "" {
		return "", fmt.Errorf("pgoverlayconnect: Repo (owner/name, e.g. $GITHUB_REPOSITORY) is required to derive the branch name from a PR or Ref")
	}
	if n := RefBranchName(opts.Repo, opts.Ref); n != "" {
		return n, nil
	}
	if opts.PR > 0 {
		return PRBranchName(opts.Repo, opts.PR), nil
	}
	return "", fmt.Errorf("pgoverlayconnect: Ref %q has no letters or digits to name a branch after; set PR", opts.Ref)
}

// Result is the resolved connection info. DSN targets the branch's Postgres
// directly (its host:port); ProxyDSN goes through the pgoverlay wire-protocol
// router (database "db@branch") and is what apps usually want — one stable
// host, routing by name.
type Result struct {
	Branch   string
	Host     string
	Port     int
	User     string
	Database string
	DSN      string
	ProxyDSN string
}

// wireBranch mirrors branchd's GET /v1/branches/{name} JSON. Password is set
// only when the server rotates credentials per branch.
type wireBranch struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	Database      string `json:"database"`
	ProxyDatabase string `json:"proxy_database"`
}

// Resolve fetches the branch's credentials from branchd and returns ready
// DSNs. In inherit mode (the server returns no password) it falls back to
// Options.Password, then $PGPASSWORD, and errors if neither is set.
func Resolve(ctx context.Context, opts Options) (Result, error) {
	if opts.Server == "" {
		return Result{}, fmt.Errorf("pgoverlayconnect: Server (PGOVERLAY_API) is required")
	}
	if opts.Token == "" {
		return Result{}, fmt.Errorf("pgoverlayconnect: Token is required")
	}
	name, err := opts.branchName()
	if err != nil {
		return Result{}, err
	}
	proxyHost, proxyPort := "", defaultProxyPort
	if opts.ProxyHost != "" {
		if proxyHost, proxyPort, err = splitProxyHost(opts.ProxyHost); err != nil {
			return Result{}, err
		}
	}

	w, err := getBranch(ctx, opts, name)
	if err != nil {
		return Result{}, err
	}

	serverHost := ""
	if u, err := url.Parse(opts.Server); err == nil {
		serverHost = u.Hostname()
	}
	r := Result{
		Branch: w.Name, Host: w.Host, Port: w.Port,
		User: w.User, Database: w.Database,
	}
	if r.Host == "" {
		r.Host = serverHost
	}
	if r.User == "" {
		r.User = "postgres"
	}
	if r.Database == "" {
		r.Database = "postgres"
	}
	password := w.Password
	if password == "" {
		password = opts.Password
		if password == "" {
			password = os.Getenv("PGPASSWORD")
		}
		if password == "" {
			return Result{}, fmt.Errorf("pgoverlayconnect: branch %q returned no password (inherit mode) and no Options.Password/PGPASSWORD set", name)
		}
	}

	if proxyHost == "" {
		proxyHost = serverHost
	}
	proxyDB := w.ProxyDatabase
	if proxyDB == "" {
		proxyDB = r.Database + "@" + w.Name
	}
	r.DSN = dsn(r.User, password, r.Host, r.Port, r.Database)
	r.ProxyDSN = dsn(r.User, password, proxyHost, proxyPort, proxyDB)
	return r, nil
}

func getBranch(ctx context.Context, opts Options, name string) (*wireBranch, error) {
	cl := opts.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(opts.Server, "/")+"/v1/branches/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+opts.Token)
	resp, err := cl.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pgoverlayconnect: GET branch %q: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("pgoverlayconnect: branch %q not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pgoverlayconnect: GET branch %q: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var w wireBranch
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("pgoverlayconnect: decode branch %q: %w", name, err)
	}
	return &w, nil
}

// defaultProxyPort is the pgoverlay router's port when ProxyHost names none.
const defaultProxyPort = 6432

// splitProxyHost parses Options.ProxyHost: "host", "host:port", "[v6]:port",
// "[v6]" or a bare IPv6 address. A port that is present must be a number in
// 1-65535.
func splitProxyHost(hp string) (string, int, error) {
	h, p, err := net.SplitHostPort(hp)
	if err != nil { // no port
		return strings.TrimSuffix(strings.TrimPrefix(hp, "["), "]"), defaultProxyPort, nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("pgoverlayconnect: ProxyHost %q: invalid port %q", hp, p)
	}
	return h, n, nil
}

// dsn builds postgres://user[:password]@host:port/db. The userinfo is
// percent-encoded the way URL parsers (pgx, libpq) decode it — a space is
// %20, never '+'. db may contain '@' (proxy routing) — legal in a URL path,
// kept literal.
func dsn(user, password, host string, port int, db string) string {
	auth := url.User(user)
	if password != "" {
		auth = url.UserPassword(user, password)
	}
	return fmt.Sprintf("postgres://%s@%s/%s", auth.String(), net.JoinHostPort(host, strconv.Itoa(port)), db)
}
