package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultHomeUnderUserHome(t *testing.T) {
	t.Setenv("PGOVERLAY_HOME", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(c.Home) != ".pgoverlay" {
		t.Fatalf("Home = %q, want ~/.pgoverlay", c.Home)
	}
	if c.RegistryPath != filepath.Join(c.Home, "pgoverlay.db") {
		t.Fatalf("RegistryPath = %q", c.RegistryPath)
	}
}

func TestHomeOverride(t *testing.T) {
	t.Setenv("PGOVERLAY_HOME", "/tmp/pgbtest")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Home != "/tmp/pgbtest" {
		t.Fatalf("Home = %q", c.Home)
	}
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// SECRETS-10: the state dir is owner-only and the registry file is created
// 0600 before SQLite opens it; installs made with the old 0755/0644 modes are
// tightened.
func TestEnsureHomeIsOwnerOnly(t *testing.T) {
	t.Setenv("PGOVERLAY_HOME", filepath.Join(t.TempDir(), "state"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if p := perm(t, c.Home); p != 0o700 {
		t.Fatalf("state dir mode %o, want 700", p)
	}
	if p := perm(t, c.RegistryPath); p != 0o600 {
		t.Fatalf("registry file mode %o, want 600", p)
	}
	// idempotent on an existing registry (does not truncate it)
	if err := os.WriteFile(c.RegistryPath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(c.RegistryPath); string(b) != "data" {
		t.Fatalf("EnsureHome rewrote an existing registry: %q", b)
	}

	// an install from before this change
	if err := os.Chmod(c.Home, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{c.RegistryPath, c.RegistryPath + "-wal"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EnsureHome(); err != nil {
		t.Fatal(err)
	}
	if p := perm(t, c.Home); p != 0o700 {
		t.Fatalf("old state dir mode %o, want tightened to 700", p)
	}
	for _, f := range []string{c.RegistryPath, c.RegistryPath + "-wal"} {
		if p := perm(t, f); p != 0o600 {
			t.Fatalf("%s mode %o, want tightened to 600", filepath.Base(f), p)
		}
	}
}
