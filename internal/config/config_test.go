package config

import (
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
