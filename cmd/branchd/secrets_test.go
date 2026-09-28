package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abd-ulbasit/pgoverlay/internal/config"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// startup mirrors the part of run() that opens the registry and configures
// at-rest encryption.
func startup(t *testing.T, token string) (*config.Config, *registry.Registry) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(cfg.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := configureSecrets(reg, cfg, "", token); err != nil {
		reg.Close()
		t.Fatal(err)
	}
	return cfg, reg
}

func rawPassword(t *testing.T, path, name string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var pw string
	if err := db.QueryRow(`SELECT password FROM branches WHERE name=?`, name).Scan(&pw); err != nil {
		t.Fatal(err)
	}
	return pw
}

// First start generates the at-rest key in the state dir (0600) and encrypts
// a plaintext password left by an older build; a restart with a rotated
// PGOVERLAY_TOKEN reuses the key and still reads the password.
func TestConfigureSecretsKeyLifecycle(t *testing.T) {
	t.Setenv("PGOVERLAY_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv(config.SecretKeyEnv, "")
	t.Setenv(config.SecretKeyPreviousEnv, "")

	// an older registry: a branch with a plaintext rotated password
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	old, err := registry.Open(cfg.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "v"}
	if err := old.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	b := &registry.Branch{Name: "pr-1", SourceID: src.ID, RWVolume: "rw"}
	if err := old.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if err := old.SetBranchPassword(b.ID, "plaintextpassword000000000000001"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	_, reg := startup(t, "first-admin-token-0001")
	fi, err := os.Stat(cfg.SecretKeyFile())
	if err != nil {
		t.Fatalf("no key generated: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %o, want 600", fi.Mode().Perm())
	}
	if raw := rawPassword(t, cfg.RegistryPath, "pr-1"); !strings.HasPrefix(raw, "enc:v2:") {
		t.Fatalf("plaintext password not encrypted at startup: %q", raw)
	}
	reg.Close()

	_, reg = startup(t, "second-admin-token-002") // token rotated
	got, err := reg.GetBranchByName("pr-1")
	if err != nil || got.PasswordUnavailable || got.Password != "plaintextpassword000000000000001" {
		t.Fatalf("after token rotation: %+v err=%v", got, err)
	}
	reg.Close()

	// the operator moves to an explicit key (e.g. from a Kubernetes Secret)
	// with a new value: the generated state-dir key is read as a previous key
	// and the row is re-encrypted under the new one
	newKey := strings.Repeat("ab", 32)
	t.Setenv(config.SecretKeyEnv, newKey)
	_, reg = startup(t, "second-admin-token-002")
	defer reg.Close()
	got, err = reg.GetBranchByName("pr-1")
	if err != nil || got.PasswordUnavailable || got.Password != "plaintextpassword000000000000001" {
		t.Fatalf("after moving to an explicit key: %+v err=%v", got, err)
	}
	parsed, err := config.ParseSecretKey(newKey)
	if err != nil {
		t.Fatal(err)
	}
	if raw := rawPassword(t, cfg.RegistryPath, "pr-1"); !strings.HasPrefix(raw, "enc:v2:"+registry.KeyID(parsed)+":") {
		t.Fatalf("row not re-encrypted under the explicit key: %q", raw)
	}
}
