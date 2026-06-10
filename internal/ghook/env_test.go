package ghook

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadEnvDefaultsAndParsing(t *testing.T) {
	ec, err := LoadEnv(env(map[string]string{
		"GHOOK_WEBHOOK_SECRET":  "wh",
		"GHOOK_PGBRANCH_SERVER": "http://pgbranch-api:7070",
		"GHOOK_PGBRANCH_TOKEN":  "tok",
		"GHOOK_SOURCE":          "main",
		"GHOOK_TTL":             "72h",
		"GHOOK_RESET_ON_PUSH":   "true",
		"GHOOK_REPOS":           "acme/widgets, acme/gadgets",
		"GHOOK_GITHUB_TOKEN":    "ghp_x",
		"GHOOK_PROXY_HOST":      "pg.example.com:30432",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if ec.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080 default", ec.Listen)
	}
	if ec.TTLSeconds != 72*3600 {
		t.Errorf("TTLSeconds = %d, want %d", ec.TTLSeconds, 72*3600)
	}
	if !ec.ResetOnPush {
		t.Error("ResetOnPush not parsed")
	}
	if len(ec.Repos) != 2 || ec.Repos[0] != "acme/widgets" || ec.Repos[1] != "acme/gadgets" {
		t.Errorf("Repos = %v", ec.Repos)
	}
	if ec.GitHubToken != "ghp_x" || ec.PGBranchServer != "http://pgbranch-api:7070" ||
		ec.PGBranchToken != "tok" || ec.ProxyHost != "pg.example.com:30432" {
		t.Errorf("config = %+v", ec)
	}
}

func TestLoadEnvRequiredVars(t *testing.T) {
	base := map[string]string{
		"GHOOK_WEBHOOK_SECRET":  "wh",
		"GHOOK_PGBRANCH_SERVER": "http://x:7070",
		"GHOOK_SOURCE":          "main",
	}
	if _, err := LoadEnv(env(base)); err != nil {
		t.Fatalf("minimal config must load: %v", err)
	}
	for _, required := range []string{"GHOOK_WEBHOOK_SECRET", "GHOOK_PGBRANCH_SERVER", "GHOOK_SOURCE"} {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		delete(m, required)
		_, err := LoadEnv(env(m))
		if err == nil || !strings.Contains(err.Error(), required) {
			t.Errorf("missing %s: err = %v, want it named", required, err)
		}
	}
}

func TestLoadEnvBadValues(t *testing.T) {
	base := map[string]string{
		"GHOOK_WEBHOOK_SECRET":  "wh",
		"GHOOK_PGBRANCH_SERVER": "http://x:7070",
		"GHOOK_SOURCE":          "main",
	}
	for k, v := range map[string]string{"GHOOK_TTL": "3 fortnights", "GHOOK_RESET_ON_PUSH": "yep"} {
		m := map[string]string{k: v}
		for bk, bv := range base {
			m[bk] = bv
		}
		if _, err := LoadEnv(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: err = %v, want it named", k, v, err)
		}
	}
}
