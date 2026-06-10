package ghook

import (
	"fmt"
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
	GitHubToken    string // GHOOK_GITHUB_TOKEN (empty = no PR comments)
	GitHubAPI      string // GHOOK_GITHUB_API, default https://api.github.com
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
	return ec, nil
}
