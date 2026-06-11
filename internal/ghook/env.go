package ghook

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// EnvConfig is the full GHOOK_* environment configuration for the
// pgbranch-github binary: the service Config plus wiring (listen address,
// pgbranch server, GitHub credentials).
type EnvConfig struct {
	Config
	Listen         string // GHOOK_LISTEN, default :8080
	PGBranchServer string // GHOOK_PGBRANCH_SERVER (required)
	PGBranchToken  string // GHOOK_PGBRANCH_TOKEN
	GitHubToken    string // GHOOK_GITHUB_TOKEN (PAT mode; empty = no comments/statuses)
	GitHubAPI      string // GHOOK_GITHUB_API, default https://api.github.com
	// GitHub App auth (mutually exclusive with GitHubToken): the service
	// mints installation tokens from the App's private key.
	AppID         string // GHOOK_APP_ID
	AppPrivateKey string // PEM, from GHOOK_APP_PRIVATE_KEY or GHOOK_APP_PRIVATE_KEY_FILE
}

// LoadEnv builds the configuration from getenv (os.Getenv in production,
// a map lookup in tests) and validates required values.
func LoadEnv(getenv func(string) string) (*EnvConfig, error) {
	ec := &EnvConfig{
		Config: Config{
			WebhookSecret: getenv("GHOOK_WEBHOOK_SECRET"),
			Source:        getenv("GHOOK_SOURCE"),
			ProxyHost:     getenv("GHOOK_PROXY_HOST"),
		},
		Listen:         getenv("GHOOK_LISTEN"),
		PGBranchServer: getenv("GHOOK_PGBRANCH_SERVER"),
		PGBranchToken:  getenv("GHOOK_PGBRANCH_TOKEN"),
		GitHubToken:    getenv("GHOOK_GITHUB_TOKEN"),
		GitHubAPI:      getenv("GHOOK_GITHUB_API"),
	}
	if ec.Listen == "" {
		ec.Listen = ":8080"
	}
	for name, val := range map[string]string{
		"GHOOK_WEBHOOK_SECRET":  ec.WebhookSecret,
		"GHOOK_PGBRANCH_SERVER": ec.PGBranchServer,
		"GHOOK_SOURCE":          ec.Source,
	} {
		if val == "" {
			return nil, fmt.Errorf("%s is required", name)
		}
	}
	if v := getenv("GHOOK_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("GHOOK_TTL: invalid duration %q (use e.g. \"72h\")", v)
		}
		ec.TTLSeconds = int(d / time.Second)
	}
	if v := getenv("GHOOK_RESET_ON_PUSH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("GHOOK_RESET_ON_PUSH: invalid bool %q", v)
		}
		ec.ResetOnPush = b
	}
	for _, r := range strings.Split(getenv("GHOOK_REPOS"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			ec.Repos = append(ec.Repos, r)
		}
	}
	switch v := getenv("GHOOK_BRANCH_NAMING"); v {
	case "", "pr-number", "git-branch":
		ec.BranchNaming = v
	default:
		return nil, fmt.Errorf("GHOOK_BRANCH_NAMING: %q (want pr-number or git-branch)", v)
	}

	// GitHub App auth: app id + private key (inline or file), exclusive
	// with the PAT.
	ec.AppID = getenv("GHOOK_APP_ID")
	ec.AppPrivateKey = getenv("GHOOK_APP_PRIVATE_KEY")
	if f := getenv("GHOOK_APP_PRIVATE_KEY_FILE"); f != "" {
		if ec.AppPrivateKey != "" {
			return nil, fmt.Errorf("GHOOK_APP_PRIVATE_KEY and GHOOK_APP_PRIVATE_KEY_FILE are mutually exclusive")
		}
		pem, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("GHOOK_APP_PRIVATE_KEY_FILE: %w", err)
		}
		ec.AppPrivateKey = string(pem)
	}
	if (ec.AppID == "") != (ec.AppPrivateKey == "") {
		return nil, fmt.Errorf("GHOOK_APP_ID and GHOOK_APP_PRIVATE_KEY (or _FILE) must be set together")
	}
	if ec.AppID != "" && ec.GitHubToken != "" {
		return nil, fmt.Errorf("GHOOK_APP_ID/GHOOK_APP_PRIVATE_KEY and GHOOK_GITHUB_TOKEN are mutually exclusive — pick App or PAT auth")
	}
	return ec, nil
}
