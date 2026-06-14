package ghook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// validSecret is a >= minWebhookSecretLen webhook secret for env tests that
// aren't exercising the length guard.
const validSecret = "0123456789abcdef" // 16 chars

func TestLoadEnvDefaultsAndParsing(t *testing.T) {
	ec, err := LoadEnv(env(map[string]string{
		"GHOOK_WEBHOOK_SECRET":  validSecret,
		"GHOOK_PGBRANCH_SERVER": "http://pgbranch-api:7070",
		"GHOOK_PGBRANCH_TOKEN":  "tok",
		"GHOOK_SOURCE":          "main",
		"GHOOK_TTL":             "72h",
		"GHOOK_RESET_ON_PUSH":   "true",
		"GHOOK_DIFF_ON_PUSH":    "true",
		"GHOOK_REPOS":           "acme/widgets, acme/gadgets",
		"GHOOK_GITHUB_TOKEN":    "ghp_x",
		"GHOOK_PROXY_HOST":      "pg.example.com:30432",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !ec.DiffOnPush {
		t.Error("DiffOnPush not parsed")
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
		"GHOOK_WEBHOOK_SECRET":  validSecret,
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

func TestLoadEnvAppAuth(t *testing.T) {
	base := map[string]string{
		"GHOOK_WEBHOOK_SECRET":  validSecret,
		"GHOOK_PGBRANCH_SERVER": "http://x:7070",
		"GHOOK_SOURCE":          "main",
	}
	with := func(extra map[string]string) func(string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return env(m)
	}

	// inline key
	ec, err := LoadEnv(with(map[string]string{
		"GHOOK_APP_ID": "12345", "GHOOK_APP_PRIVATE_KEY": "-----BEGIN RSA PRIVATE KEY-----\n...",
	}))
	if err != nil {
		t.Fatalf("app auth config must load: %v", err)
	}
	if ec.AppID != "12345" || !strings.HasPrefix(ec.AppPrivateKey, "-----BEGIN") {
		t.Errorf("app config = %q / %q", ec.AppID, ec.AppPrivateKey)
	}

	// key from file
	f := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(f, []byte("-----BEGIN PRIVATE KEY-----\nfromfile"), 0o600); err != nil {
		t.Fatal(err)
	}
	ec, err = LoadEnv(with(map[string]string{"GHOOK_APP_ID": "12345", "GHOOK_APP_PRIVATE_KEY_FILE": f}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ec.AppPrivateKey, "fromfile") {
		t.Errorf("AppPrivateKey = %q, want file contents", ec.AppPrivateKey)
	}

	// invalid combinations
	for name, extra := range map[string]map[string]string{
		"app id without key": {"GHOOK_APP_ID": "12345"},
		"key without app id": {"GHOOK_APP_PRIVATE_KEY": "pem"},
		"key and key file":   {"GHOOK_APP_ID": "1", "GHOOK_APP_PRIVATE_KEY": "pem", "GHOOK_APP_PRIVATE_KEY_FILE": f},
		"app auth and PAT":   {"GHOOK_APP_ID": "1", "GHOOK_APP_PRIVATE_KEY": "pem", "GHOOK_GITHUB_TOKEN": "ghp_x"},
		"missing key file":   {"GHOOK_APP_ID": "1", "GHOOK_APP_PRIVATE_KEY_FILE": filepath.Join(t.TempDir(), "nope.pem")},
	} {
		if _, err := LoadEnv(with(extra)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// TestLoadEnvWebhookSecretLength: an empty secret is rejected as missing, a
// too-short one is rejected as weak, and a >= minWebhookSecretLen one loads.
func TestLoadEnvWebhookSecretLength(t *testing.T) {
	base := func(secret string) func(string) string {
		return env(map[string]string{
			"GHOOK_WEBHOOK_SECRET":  secret,
			"GHOOK_PGBRANCH_SERVER": "http://x:7070",
			"GHOOK_SOURCE":          "main",
		})
	}
	// empty -> "required"
	if _, err := LoadEnv(base("")); err == nil || !strings.Contains(err.Error(), "GHOOK_WEBHOOK_SECRET") {
		t.Errorf("empty secret: err = %v, want it named required", err)
	}
	// short (15 chars) -> length error naming the var
	short := strings.Repeat("a", minWebhookSecretLen-1)
	_, err := LoadEnv(base(short))
	if err == nil || !strings.Contains(err.Error(), "GHOOK_WEBHOOK_SECRET") {
		t.Errorf("short secret: err = %v, want a length rejection naming the var", err)
	}
	// exactly minWebhookSecretLen -> ok
	if _, err := LoadEnv(base(strings.Repeat("a", minWebhookSecretLen))); err != nil {
		t.Errorf("min-length secret must load: %v", err)
	}
}

func TestLoadEnvBadValues(t *testing.T) {
	base := map[string]string{
		"GHOOK_WEBHOOK_SECRET":  validSecret,
		"GHOOK_PGBRANCH_SERVER": "http://x:7070",
		"GHOOK_SOURCE":          "main",
	}
	for k, v := range map[string]string{"GHOOK_TTL": "3 fortnights", "GHOOK_RESET_ON_PUSH": "yep", "GHOOK_DIFF_ON_PUSH": "nope"} {
		m := map[string]string{k: v}
		for bk, bv := range base {
			m[bk] = bv
		}
		if _, err := LoadEnv(env(m)); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: err = %v, want it named", k, v, err)
		}
	}
}
