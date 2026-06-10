package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Home          string // state directory, default ~/.pgbranch
	RegistryPath  string // SQLite file
	PostgresImage string // default image for helpers/branches when source has no version
}

func Load() (*Config, error) {
	home := os.Getenv("PGBRANCH_HOME")
	if home == "" {
		uh, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(uh, ".pgbranch")
	}
	return &Config{
		Home:          home,
		RegistryPath:  filepath.Join(home, "pgbranch.db"),
		PostgresImage: "postgres:17",
	}, nil
}

// EnsureHome creates the state directory.
func (c *Config) EnsureHome() error { return os.MkdirAll(c.Home, 0o755) }
