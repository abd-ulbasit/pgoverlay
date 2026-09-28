package pgoverlayconnect

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// stub serves GET /v1/branches/{name}, echoing the supplied wireBranch and
// recording the Authorization header + requested name.
func stub(t *testing.T, w wireBranch) (*httptest.Server, *string, *string) {
	t.Helper()
	var gotAuth, gotName string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotName = strings.TrimPrefix(r.URL.Path, "/v1/branches/")
		if w.Name == "" { // simulate not found
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(rw).Encode(w)
	}))
	t.Cleanup(srv.Close)
	return srv, &gotAuth, &gotName
}

func TestResolveRotatedPassword(t *testing.T) {
	srv, auth, name := stub(t, wireBranch{
		Name: "gh-d782c8-feat-login", Host: "10.0.0.5", Port: 5432, User: "app",
		Password: "rot123", Database: "appdb", ProxyDatabase: "appdb@gh-d782c8-feat-login",
	})
	res, err := Resolve(context.Background(), Options{
		Server: srv.URL, Token: "tok", Repo: "acme/widgets", Ref: "feat/Login", ProxyHost: "proxy.example.com:6432",
	})
	if err != nil {
		t.Fatal(err)
	}
	if *auth != "Bearer tok" {
		t.Errorf("auth = %q", *auth)
	}
	if *name != "gh-d782c8-feat-login" {
		t.Errorf("requested name = %q, want the pgoverlay-github name gh-d782c8-feat-login", *name)
	}
	if res.DSN != "postgres://app:rot123@10.0.0.5:5432/appdb" {
		t.Errorf("DSN = %q", res.DSN)
	}
	if res.ProxyDSN != "postgres://app:rot123@proxy.example.com:6432/appdb@gh-d782c8-feat-login" {
		t.Errorf("ProxyDSN = %q", res.ProxyDSN)
	}
}

// Resolve looks up exactly the name pgoverlay-github creates for a pull
// request: Repo+PR is the pr-number name, Repo+Ref the git-branch name, and a
// Ref without letters or digits falls back to the PR number as the service
// does.
func TestResolveDerivesTheGitHubAppName(t *testing.T) {
	srv, _, name := stub(t, wireBranch{Name: "x", Host: "h", Port: 5432, Password: "p"})
	cases := []struct {
		opts Options
		want string
	}{
		{Options{Repo: "acme/widgets", PR: 7}, "gh-d782c8-pr-7"},
		{Options{Repo: "Acme/Widgets", PR: 7}, "gh-d782c8-pr-7"},
		{Options{Repo: "acme/gadgets", PR: 7}, "gh-9c2435-pr-7"},
		{Options{Repo: "acme/widgets", Ref: "feat/Login"}, "gh-d782c8-feat-login"},
		{Options{Repo: "acme/widgets", Ref: "feat/Login", PR: 7}, "gh-d782c8-feat-login"},
		{Options{Repo: "acme/widgets", Ref: "-/-", PR: 9}, "gh-d782c8-pr-9"},
		{Options{Branch: "exact-name", Repo: "acme/widgets", PR: 7}, "exact-name"},
	}
	for _, tc := range cases {
		tc.opts.Server, tc.opts.Token = srv.URL, "t"
		if _, err := Resolve(context.Background(), tc.opts); err != nil {
			t.Fatalf("%+v: %v", tc.opts, err)
		}
		if *name != tc.want {
			t.Errorf("%+v: requested %q, want %q", tc.opts, *name, tc.want)
		}
	}
}

func TestResolveProxyHostForms(t *testing.T) {
	srv, _, _ := stub(t, wireBranch{Name: "b", Host: "fd00::5", Port: 31234, User: "u",
		Password: "p", Database: "db", ProxyDatabase: "db@b"})
	cases := map[string]string{
		"":                  "postgres://u:p@127.0.0.1:6432/db@b", // the server host
		"proxy":             "postgres://u:p@proxy:6432/db@b",
		"proxy:7000":        "postgres://u:p@proxy:7000/db@b",
		"[::1]:6433":        "postgres://u:p@[::1]:6433/db@b",
		"[::1]":             "postgres://u:p@[::1]:6432/db@b",
		"fd00::1":           "postgres://u:p@[fd00::1]:6432/db@b",
		"pg.example.com:80": "postgres://u:p@pg.example.com:80/db@b",
	}
	for proxy, want := range cases {
		res, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "b", ProxyHost: proxy})
		if err != nil {
			t.Fatalf("ProxyHost %q: %v", proxy, err)
		}
		if res.ProxyDSN != want {
			t.Errorf("ProxyHost %q: ProxyDSN = %q, want %q", proxy, res.ProxyDSN, want)
		}
		if res.DSN != "postgres://u:p@[fd00::5]:31234/db" {
			t.Errorf("DSN = %q, want the IPv6 branch host bracketed", res.DSN)
		}
	}
	for _, bad := range []string{"proxy:abc", "proxy:", "proxy:0", "proxy:70000", "[::1]:x"} {
		if _, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "b", ProxyHost: bad}); err == nil || !strings.Contains(err.Error(), "invalid port") {
			t.Errorf("ProxyHost %q: err = %v, want invalid port", bad, err)
		}
	}
}

func TestResolveInheritModeRequiresPassword(t *testing.T) {
	srv, _, _ := stub(t, wireBranch{
		Name: "main-stable", Host: "h", Port: 5432, User: "postgres",
		Database: "postgres", ProxyDatabase: "postgres@main-stable", // no Password
	})
	t.Setenv("PGPASSWORD", "")
	if _, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "main-stable"}); err == nil || !strings.Contains(err.Error(), "no password") {
		t.Fatalf("err = %v, want inherit-mode no-password error", err)
	}
	// supplied password is used in inherit mode
	res, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "main-stable", Password: "fromenv"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.DSN, ":fromenv@") {
		t.Errorf("DSN = %q, want supplied password", res.DSN)
	}
}

func TestResolveNotFound(t *testing.T) {
	srv, _, _ := stub(t, wireBranch{}) // empty Name => 404
	if _, err := Resolve(context.Background(), Options{Server: srv.URL, Token: "t", Branch: "nope"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found", err)
	}
}

func TestResolveValidation(t *testing.T) {
	if _, err := Resolve(context.Background(), Options{Token: "t", Branch: "b"}); err == nil {
		t.Error("want error without Server")
	}
	if _, err := Resolve(context.Background(), Options{Server: "http://x", Branch: "b"}); err == nil {
		t.Error("want error without Token")
	}
	if _, err := Resolve(context.Background(), Options{Server: "http://x", Token: "t"}); err == nil {
		t.Error("want error without Branch, PR or Ref")
	}
	// A PR or Ref names a branch only together with its repository.
	for _, o := range []Options{{PR: 7}, {Ref: "feat/login"}} {
		o.Server, o.Token = "http://x", "t"
		if _, err := Resolve(context.Background(), o); err == nil || !strings.Contains(err.Error(), "Repo") {
			t.Errorf("%+v: err = %v, want Repo required", o, err)
		}
	}
	if _, err := Resolve(context.Background(), Options{Server: "http://x", Token: "t", Repo: "a/b", Ref: "-/-"}); err == nil || !strings.Contains(err.Error(), "set PR") {
		t.Errorf("ref without letters or digits: err = %v, want a hint to set PR", err)
	}
}

// goldenNames is testdata/branch_names.json, shared with internal/ghook's and
// sdk/js-connect's tests.
type goldenNames struct {
	RepoKeys []struct{ Repo, Key string } `json:"repo_keys"`
	Names    []struct {
		Repo string
		PR   int
		Ref  string
		Want string
	} `json:"names"`
	Sanitize []struct{ Ref, Want string } `json:"sanitize"`
}

func loadGolden(t *testing.T) goldenNames {
	t.Helper()
	data, err := os.ReadFile("testdata/branch_names.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenNames
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if len(g.RepoKeys) == 0 || len(g.Names) == 0 || len(g.Sanitize) == 0 {
		t.Fatal("golden table has an empty section")
	}
	return g
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func TestBranchNamesGolden(t *testing.T) {
	g := loadGolden(t)
	for _, c := range g.RepoKeys {
		if got := RepoKey(c.Repo); got != c.Key {
			t.Errorf("RepoKey(%q) = %q, want %q", c.Repo, got, c.Key)
		}
	}
	for _, c := range g.Names {
		got, err := Options{Repo: c.Repo, PR: c.PR, Ref: c.Ref}.branchName()
		if err != nil || got != c.Want {
			t.Errorf("name(repo=%q pr=%d ref=%q) = %q, %v; want %q", c.Repo, c.PR, c.Ref, got, err, c.Want)
		}
		if !validName.MatchString(got) {
			t.Errorf("name %q is not a valid branch name", got)
		}
		if c.Ref == "" {
			if got := PRBranchName(c.Repo, c.PR); got != c.Want {
				t.Errorf("PRBranchName(%q, %d) = %q, want %q", c.Repo, c.PR, got, c.Want)
			}
		}
	}
	for _, c := range g.Sanitize {
		if got := SanitizeRef(c.Ref); got != c.Want {
			t.Errorf("SanitizeRef(%q) = %q, want %q", c.Ref, got, c.Want)
		}
	}
}

// Every derived name satisfies the engine's name rule, whatever the input:
// never over 41 characters (SanitizeRef used to return 42 for a 40-char run
// followed by a separator and one more character).
func TestDerivedNamesAlwaysValid(t *testing.T) {
	refs := []string{strings.Repeat("a", 40) + "/b", strings.Repeat("a/", 40), strings.Repeat("x", 200),
		strings.Repeat("ab-", 20), "a", "9/9", strings.Repeat("é", 50) + "z"}
	for _, ref := range refs {
		if s := SanitizeRef(ref); len(s) > MaxBranchNameLen || (s != "" && !validName.MatchString(s)) {
			t.Errorf("SanitizeRef(%q) = %q (%d chars)", ref, s, len(s))
		}
		for _, repo := range []string{"acme/widgets", "", "a-very-long-organisation/a-very-long-repository-name"} {
			if n := RefBranchName(repo, ref); !validName.MatchString(n) {
				t.Errorf("RefBranchName(%q, %q) = %q (%d chars)", repo, ref, n, len(n))
			}
		}
	}
	if n := PRBranchName("acme/widgets", 2147483647); !validName.MatchString(n) {
		t.Errorf("PRBranchName with a large number = %q", n)
	}
}

// The JavaScript helper must produce the same names. When Node 18+ is on
// PATH (it is on GitHub's ubuntu runners), run its golden test here too, so
// `go test ./...` catches a drift in either implementation.
func TestJSConnectMatchesGolden(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; run `node --test sdk/js-connect/test/` for the JavaScript side")
	}
	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		t.Skipf("node --version: %v", err)
	}
	var major int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "v%d", &major); err != nil || major < 18 {
		t.Skipf("node %s is older than 18", strings.TrimSpace(string(out)))
	}
	cmd := exec.Command(node, "--test", "test/names.test.mjs")
	cmd.Dir = "../sdk/js-connect"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sdk/js-connect golden test failed: %v\n%s", err, out)
	}
}
