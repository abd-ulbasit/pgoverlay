package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// branchServer serves one branch for GET /v1/branches/{name}.
func branchServer(t *testing.T, b api.Branch) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(b)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("PGOVERLAY_TOKEN", "tok")
	return ts.URL
}

func readyBranch() api.Branch {
	return api.Branch{
		Name: "pr-9", State: "ready", Host: "10.0.0.7", Port: 32788,
		User: "postgres", Database: "postgres", ProxyDatabase: "postgres@pr-9",
	}
}

// CLI-05: a failed branch has no container, so connect used to print a DSN
// with port 0; a creating or resetting one does not accept connections yet.
func TestConnectRefusesBranchesThatAreNotReady(t *testing.T) {
	for state, hint := range map[string]string{
		"failed":    "pgb history pr-9",
		"creating":  "wait until",
		"resetting": "wait until",
	} {
		b := readyBranch()
		b.State, b.Port, b.Host = state, 0, ""
		out, err := runErr(t, "connect", "pr-9", "--server", branchServer(t, b))
		wantErr(t, err, `branch "pr-9" is `+state+", not ready", hint)
		if strings.Contains(out, "postgres://") {
			t.Fatalf("%s: printed a DSN anyway: %q", state, out)
		}
	}
}

// CLI-02: the proxy URL uses the router address branchd advertises instead of
// "<API host>:6432".
func TestConnectUsesAdvertisedProxyEndpoint(t *testing.T) {
	b := readyBranch()
	b.ProxyHost, b.ProxyPort = "pgoverlay-proxy.pgoverlay-system", 5433
	out := run(t, "connect", "pr-9", "--server", branchServer(t, b))
	if !strings.Contains(out, "postgres://postgres@pgoverlay-proxy.pgoverlay-system:5433/postgres@pr-9\n") {
		t.Fatalf("proxy URL ignores the advertised endpoint: %q", out)
	}

	// port only: keep the API host
	b.ProxyHost = ""
	srv := branchServer(t, b)
	u, _ := url.Parse(srv)
	out = run(t, "connect", "pr-9", "--server", srv)
	if !strings.Contains(out, "@"+u.Hostname()+":5433/postgres@pr-9\n") {
		t.Fatalf("proxy URL = %q, want the API host with the advertised port", out)
	}
}

func TestConnectProxyFlagsOverrideServer(t *testing.T) {
	b := readyBranch()
	b.ProxyHost, b.ProxyPort = "pgoverlay-proxy.pgoverlay-system", 5433
	out := run(t, "connect", "pr-9", "--proxy-host", "pg.example.com", "--proxy-port", "30432", "--server", branchServer(t, b))
	if !strings.Contains(out, "@pg.example.com:30432/postgres@pr-9\n") {
		t.Fatalf("proxy flags ignored: %q", out)
	}
	// the direct URL is unaffected
	if !strings.Contains(out, "postgres://postgres@10.0.0.7:32788/postgres\n") {
		t.Fatalf("direct URL changed: %q", out)
	}
	_, err := runErr(t, "connect", "pr-9", "--proxy-port", "70000", "--server", branchServer(t, b))
	wantErr(t, err, "--proxy-port 70000")
}

// CLI-08: user and database were interpolated raw and the password was
// form-encoded (space as '+'), so "app@corp" split into user "app" and host
// "corp@...".
func TestConnectPercentEncodesUserPasswordAndDatabase(t *testing.T) {
	b := readyBranch()
	b.User, b.Password, b.Database, b.ProxyDatabase = "app@corp", "p w+/:@x", "my db", "my db@pr-9"
	b.Host = "fd00::7"
	out := run(t, "connect", "pr-9", "--server", branchServer(t, b))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("output %q", out)
	}
	direct, proxy := lines[0], lines[1]
	if direct != "postgres://app%40corp:p%20w+%2F%3A%40x@[fd00::7]:32788/my%20db" {
		t.Fatalf("direct URL = %q", direct)
	}
	if strings.Contains(direct, "p+w") {
		t.Fatalf("space encoded as '+': %q", direct)
	}
	for _, raw := range []string{direct, proxy} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%q does not parse: %v", raw, err)
		}
		pw, _ := u.User.Password()
		if u.User.Username() != "app@corp" || pw != "p w+/:@x" {
			t.Fatalf("%q decodes to user %q password %q", raw, u.User.Username(), pw)
		}
	}
	if u, _ := url.Parse(proxy); u.Path != "/my db@pr-9" {
		t.Fatalf("proxy database decodes to %q, want %q", u.Path, "/my db@pr-9")
	}
}

// Local mode reads only the registry (no container runtime) and applies the
// same readiness and encoding rules.
func TestLocalModeConnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PGOVERLAY_HOME", home)
	t.Setenv("PGOVERLAY_SERVER", "")
	reg, err := registry.Open(filepath.Join(home, "pgoverlay.db"))
	if err != nil {
		t.Fatal(err)
	}
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "v", ConnUser: "app@corp", ConnDB: "my db"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	ready := &registry.Branch{Name: "pr-1", SourceID: src.ID, RWVolume: "rw1", SourceVolume: "v"}
	failed := &registry.Branch{Name: "pr-2", SourceID: src.ID, RWVolume: "rw2", SourceVolume: "v"}
	for _, b := range []*registry.Branch{ready, failed} {
		if err := reg.CreateBranch(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.MarkBranchReady(ready.ID, "cid", "127.0.0.1", 5555); err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionBranch(failed.ID, registry.BranchFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	reg.Close()

	if out := run(t, "connect", "pr-1"); out != "postgres://app%40corp@127.0.0.1:5555/my%20db\n" {
		t.Fatalf("local connect = %q", out)
	}
	_, err = runErr(t, "connect", "pr-2")
	wantErr(t, err, `branch "pr-2" is failed, not ready`)
}
