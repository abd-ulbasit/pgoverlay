// Package pgbranchconnect resolves a ready Postgres connection string for a
// pgbranch branch by asking branchd for the branch's current credentials.
//
// It exists to reconcile two pgbranch features that are otherwise in tension:
// per-branch credential rotation (each branch has its own password) and
// static application configuration (a 12-factor app holds fixed env vars, not
// a per-branch password). With this helper the static config an app holds is
// the branchd API endpoint plus a scoped (viewer) token; the per-branch
// password is fetched at startup:
//
//	res, err := pgbranchconnect.Resolve(ctx, pgbranchconnect.Options{
//		Server: os.Getenv("PGBRANCH_API"),    // https://branchd:7070
//		Token:  os.Getenv("PGBRANCH_TOKEN"),  // a viewer token is enough
//		Ref:    os.Getenv("GIT_REF"),         // e.g. "feat/login" -> feat-login
//	})
//	db, _ := sql.Open("pgx", res.ProxyDSN)
//
// The package is self-contained (stdlib only) and never imports pgbranch
// internals: it speaks the branchd REST API directly.
package pgbranchconnect

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

// Options configures Resolve. Exactly one of Branch or Ref identifies the
// branch; Ref is sanitized the same way pgbranch's ghook names branches from a
// git ref (lowercase, non-alphanumerics collapse to single dashes, trimmed,
// ≤41 chars), so an app and the webhook agree on the name with no coordination.
type Options struct {
	Server    string // branchd base URL (PGBRANCH_API), required
	Token     string // API bearer token (a viewer token suffices), required
	Branch    string // exact branch name; or set Ref
	Ref       string // git ref, sanitized to a branch name; used when Branch == ""
	ProxyHost string // host[:port] of the pgbranch router for ProxyDSN; "" => server host + :6432
	Password  string // fallback password for inherit-mode branches (else PGPASSWORD)
	HTTP      *http.Client
}

// Result is the resolved connection info. DSN targets the branch's Postgres
// directly (its host:port); ProxyDSN goes through the pgbranch wire-protocol
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
		return Result{}, fmt.Errorf("pgbranchconnect: Server (PGBRANCH_API) is required")
	}
	if opts.Token == "" {
		return Result{}, fmt.Errorf("pgbranchconnect: Token is required")
	}
	name := opts.Branch
	if name == "" {
		name = SanitizeRef(opts.Ref)
	}
	if name == "" {
		return Result{}, fmt.Errorf("pgbranchconnect: a Branch or Ref is required")
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
			return Result{}, fmt.Errorf("pgbranchconnect: branch %q returned no password (inherit mode) and no Options.Password/PGPASSWORD set", name)
		}
	}

	proxyHost, proxyPort := serverHost, 6432
	if opts.ProxyHost != "" {
		if h, p, err := net.SplitHostPort(opts.ProxyHost); err == nil {
			proxyHost = h
			if n, err := strconv.Atoi(p); err == nil {
				proxyPort = n
			}
		} else {
			proxyHost = opts.ProxyHost
		}
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
		return nil, fmt.Errorf("pgbranchconnect: GET branch %q: %w", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("pgbranchconnect: branch %q not found", name)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pgbranchconnect: GET branch %q: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var w wireBranch
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("pgbranchconnect: decode branch %q: %w", name, err)
	}
	return &w, nil
}

// dsn builds postgres://user[:password]@host:port/db. db may contain '@'
// (proxy routing) — legal in a URL path, kept literal.
func dsn(user, password, host string, port int, db string) string {
	auth := url.QueryEscape(user)
	if password != "" {
		auth += ":" + url.QueryEscape(password)
	}
	return fmt.Sprintf("postgres://%s@%s/%s", auth, net.JoinHostPort(host, strconv.Itoa(port)), db)
}

// SanitizeRef maps a git ref to a pgbranch branch name (^[a-z0-9][a-z0-9-]{0,40}$),
// matching ghook's git-branch naming: lowercase, runs of other characters
// collapse to single dashes, edges trimmed, truncated to 41 chars.
func SanitizeRef(ref string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(ref) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= 41 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
