package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/client"
	"github.com/docker/go-connections/tlsconfig"
)

// cliContext is the docker CLI context in effect (see currentCLIContext).
type cliContext struct {
	Name          string
	Host          string // the context's docker endpoint, e.g. unix:///..., tcp://..., ssh://...
	SkipTLSVerify bool
	TLSDir        string // holds ca.pem / cert.pem / key.pem; "" when the context stores none
}

// dockerConfigDir is $DOCKER_CONFIG, else ~/.docker (the docker CLI's rule).
func dockerConfigDir() (string, error) {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker"), nil
}

// currentCLIContext resolves the docker CLI context the same way the docker
// CLI does when DOCKER_HOST is unset: $DOCKER_CONTEXT, else currentContext in
// config.json. It returns the zero value for the built-in "default" context
// or when nothing is configured. A context named by DOCKER_CONTEXT that cannot
// be read is an error; an unreadable config.json or stale currentContext is
// ignored, as before, and the SDK default is used.
func currentCLIContext() (cliContext, error) {
	dir, err := dockerConfigDir()
	if err != nil {
		return cliContext{}, nil
	}
	name, explicit := os.Getenv("DOCKER_CONTEXT"), true
	if name == "" {
		explicit = false
		cfg := struct {
			CurrentContext string `json:"currentContext"`
		}{}
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil || json.Unmarshal(raw, &cfg) != nil {
			return cliContext{}, nil
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return cliContext{}, nil
	}
	sum := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(sum[:])
	meta := struct {
		Endpoints map[string]struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"Endpoints"`
	}{}
	raw, err := os.ReadFile(filepath.Join(dir, "contexts", "meta", id, "meta.json"))
	if err == nil {
		err = json.Unmarshal(raw, &meta)
	}
	if err != nil {
		if explicit {
			return cliContext{}, fmt.Errorf("docker context %q (DOCKER_CONTEXT): %w", name, err)
		}
		return cliContext{}, nil
	}
	ep := meta.Endpoints["docker"]
	c := cliContext{Name: name, Host: ep.Host, SkipTLSVerify: ep.SkipTLSVerify}
	tlsDir := filepath.Join(dir, "contexts", "tls", id, "docker")
	if _, err := os.Stat(tlsDir); err == nil {
		c.TLSDir = tlsDir
	}
	return c, nil
}

// DockerHostFromCLIContext resolves the docker endpoint from the CLI's
// current context ($DOCKER_CONTEXT, else ~/.docker/config.json, honouring
// $DOCKER_CONFIG). The Go SDK's FromEnv only honors DOCKER_HOST, so without
// this, setups like Colima (where /var/run/docker.sock is absent or stale)
// fail. Returns "" when unresolvable. The endpoint is returned as stored, so
// it may be an ssh:// URL that the SDK cannot dial (NewDockerDriver rejects
// those). Exported so integration helpers can prime DOCKER_HOST for libraries
// (e.g. testcontainers) that don't read docker CLI contexts.
func DockerHostFromCLIContext() string {
	c, _ := currentCLIContext()
	return c.Host
}

// checkDockerHost rejects endpoints the Docker SDK cannot dial. The SDK
// accepts any scheme and treats every one but unix and npipe as plain TCP, so
// an ssh:// endpoint silently became an HTTP request to port 80 of the ssh
// host ("dial tcp: lookup thinkpad: no such host" or whatever listens there).
func checkDockerHost(host, origin string) error {
	scheme, _, ok := strings.Cut(host, "://")
	if !ok {
		return nil // let the SDK report its own parse error
	}
	switch scheme {
	case "unix", "npipe", "tcp", "http", "https":
		return nil
	case "ssh":
		return fmt.Errorf("docker endpoint %s (from %s) uses ssh://, which pgoverlay cannot dial. "+
			"Run branchd on the Docker host and use pgb --server, or expose the daemon as a unix:// or tcp:// endpoint "+
			"and set DOCKER_HOST (for example `ssh -NL /tmp/pgoverlay-docker.sock:/var/run/docker.sock HOST` "+
			"and DOCKER_HOST=unix:///tmp/pgoverlay-docker.sock). Branch ports are published on the Docker host's "+
			"127.0.0.1, so their connection strings only work on that host", host, origin)
	default:
		return fmt.Errorf("docker endpoint %s (from %s): unsupported scheme %q (want unix://, tcp:// or npipe://)", host, origin, scheme)
	}
}

// contextTLSOpt returns a client option carrying the context's TLS material
// (what `docker context create --docker "host=tcp://...,ca=...,cert=...,key=..."`
// stores), or nil when the context has none.
func contextTLSOpt(c cliContext) (client.Opt, error) {
	if c.TLSDir == "" && !c.SkipTLSVerify {
		return nil, nil
	}
	opts := tlsconfig.Options{InsecureSkipVerify: c.SkipTLSVerify, ExclusiveRootPools: true}
	if c.TLSDir != "" {
		opts.CAFile = existing(filepath.Join(c.TLSDir, "ca.pem"))
		opts.CertFile = existing(filepath.Join(c.TLSDir, "cert.pem"))
		opts.KeyFile = existing(filepath.Join(c.TLSDir, "key.pem"))
	}
	cfg, err := tlsconfig.Client(opts)
	if err != nil {
		return nil, fmt.Errorf("docker context %q TLS material: %w", c.Name, err)
	}
	return client.WithHTTPClient(&http.Client{
		Transport:     &http.Transport{TLSClientConfig: cfg},
		CheckRedirect: client.CheckRedirect,
	}), nil
}

// existing returns path when the file exists, else "".
func existing(path string) string {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return path
}
