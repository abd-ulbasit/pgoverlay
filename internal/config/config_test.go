package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultHomeUnderUserHome(t *testing.T) {
	t.Setenv("PGBRANCH_HOME", "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(c.Home) != ".pgbranch" {
		t.Fatalf("Home = %q, want ~/.pgbranch", c.Home)
	}
	if c.RegistryPath != filepath.Join(c.Home, "pgbranch.db") {
		t.Fatalf("RegistryPath = %q", c.RegistryPath)
	}
}

func TestHomeOverride(t *testing.T) {
	t.Setenv("PGBRANCH_HOME", "/tmp/pgbtest")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Home != "/tmp/pgbtest" {
		t.Fatalf("Home = %q", c.Home)
	}
}
