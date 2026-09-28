package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// unreachable is a server that fails the test if any request reaches it:
// validation errors must be reported before anything is sent.
func unreachable(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request reached the server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// localMode points the CLI at an empty home with no server, so a command
// that got past validation would try to open the registry and Docker.
func localMode(t *testing.T) {
	t.Helper()
	t.Setenv("PGOVERLAY_HOME", t.TempDir())
	t.Setenv("PGOVERLAY_SERVER", "")
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1") // never dialled by a rejected command
}

func wantErr(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want one containing %q", fragments)
	}
	for _, f := range fragments {
		if !strings.Contains(err.Error(), f) {
			t.Fatalf("error %q does not contain %q", err, f)
		}
	}
}

// #16: `--host ""` was accepted (MarkFlagRequired only checks presence), and
// pg_basebackup then fell back to a Unix socket.
func TestSourceAddRejectsEmptyRequiredValues(t *testing.T) {
	t.Setenv("PGPASSWORD", "secret")
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	srv := unreachable(t)
	cases := map[string]struct {
		args []string
		want []string
	}{
		"empty host":       {[]string{"--host", ""}, []string{"--host must not be empty", "host.docker.internal"}},
		"blank host":       {[]string{"--host", "  "}, []string{"--host must not be empty"}},
		"empty user":       {[]string{"--host", "db", "--user", ""}, []string{"--user must not be empty"}},
		"empty database":   {[]string{"--host", "db", "--database", ""}, []string{"--database must not be empty"}},
		"empty pg-version": {[]string{"--host", "db", "--pg-version", ""}, []string{"--pg-version must not be empty"}},
		"port zero":        {[]string{"--host", "db", "--port", "0"}, []string{"--port 0", "1-65535"}},
		"port too big":     {[]string{"--host", "db", "--port", "70000"}, []string{"--port 70000"}},
	}
	for name, tc := range cases {
		for _, mode := range []string{"server", "local"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				args := append([]string{"source", "add", "main"}, tc.args...)
				if mode == "server" {
					args = append(args, "--server", srv)
				} else {
					localMode(t)
				}
				_, err := runErr(t, args...)
				wantErr(t, err, tc.want...)
			})
		}
	}
}

// CLI-04: any --via other than "dump" used to fall through to pg_basebackup
// in local mode and was stored verbatim.
func TestSourceAddRejectsUnknownVia(t *testing.T) {
	t.Setenv("PGPASSWORD", "secret")
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	srv := unreachable(t)
	for _, via := range []string{"Dump", "pg_dump", "basebackup ", ""} {
		for _, mode := range []string{"server", "local"} {
			args := []string{"source", "add", "prod", "--host", "db.example", "--via", via}
			if mode == "server" {
				args = append(args, "--server", srv)
			} else {
				localMode(t)
			}
			_, err := runErr(t, args...)
			wantErr(t, err, "invalid --via", `"basebackup"`, `"dump"`)
		}
	}
}

func TestEmptyPositionalArgumentsAreRejected(t *testing.T) {
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	srv := unreachable(t)
	cases := map[string]struct {
		args []string
		want string
	}{
		"connect":         {[]string{"connect", ""}, "pgb connect: NAME must not be empty"},
		"branch create":   {[]string{"branch", "create", " ", "--from", "main"}, "pgb branch create: NAME must not be empty"},
		"branch destroy":  {[]string{"branch", "destroy", ""}, "NAME must not be empty"},
		"branch reset":    {[]string{"branch", "reset", ""}, "NAME must not be empty"},
		"branch recover":  {[]string{"branch", "recover", ""}, "NAME must not be empty"},
		"history":         {[]string{"history", ""}, "NAME must not be empty"},
		"diff":            {[]string{"diff", ""}, "NAME must not be empty"},
		"source rm":       {[]string{"source", "rm", ""}, "NAME must not be empty"},
		"source refresh":  {[]string{"source", "refresh", ""}, "NAME must not be empty"},
		"source get-mask": {[]string{"source", "get-mask", ""}, "NAME must not be empty"},
		"set-mask file":   {[]string{"source", "set-mask", "main", ""}, "FILE must not be empty"},
		"token create":    {[]string{"token", "create", ""}, "NAME must not be empty"},
		"token revoke":    {[]string{"token", "revoke", ""}, "NAME must not be empty"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runErr(t, append(tc.args, "--server", srv)...)
			wantErr(t, err, tc.want)
		})
	}
}

// CLI-07: a --server without a scheme failed deep inside net/http with
// `unsupported protocol scheme "localhost"`.
func TestServerFlagMustBeHTTPURL(t *testing.T) {
	for _, bad := range []string{"localhost:7070", "branchd.example", "ftp://branchd", "http://"} {
		_, err := runErr(t, "branch", "ls", "--server", bad)
		wantErr(t, err, "invalid --server", "http://")
		if strings.Contains(err.Error(), "unsupported protocol scheme") {
			t.Fatalf("%q: transport error leaked instead of a validation message: %v", bad, err)
		}
	}
	// the env var is checked the same way
	t.Setenv("PGOVERLAY_SERVER", "localhost:7070")
	_, err := runErr(t, "branch", "ls")
	wantErr(t, err, "PGOVERLAY_SERVER", "localhost:7070")

	// ...but `pgb version` still works with a broken environment
	if _, err := runErr(t, "version"); err != nil {
		t.Fatalf("pgb version with a bad PGOVERLAY_SERVER: %v", err)
	}
}

func TestBranchCreateRejectsNegativeTTL(t *testing.T) {
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	_, err := runErr(t, "branch", "create", "pr-1", "--from", "main", "--ttl", "-1h", "--server", unreachable(t))
	wantErr(t, err, "--ttl", "negative")
}

func TestServerModeWarnsWhenTokenUnset(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[]"))
	}))
	defer ts.Close()
	t.Setenv("PGOVERLAY_TOKEN", "")
	out, err := runErr(t, "branch", "ls", "--server", ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PGOVERLAY_TOKEN is not set") {
		t.Fatalf("no warning about the missing token in %q", out)
	}
}
