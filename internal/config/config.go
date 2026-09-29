package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
	Home          string // state directory, default ~/.pgoverlay
	RegistryPath  string // SQLite file
	PostgresImage string // default image for helpers/branches when source has no version
	// LazyRW is whether overlay branches use the lazyrw shim, which copies a
	// relation file into the branch on its first write instead of on open:
	// "on" or "off" ($PGOVERLAY_LAZYRW; "" = on; branchd --lazyrw overrides
	// it). Parse it with ParseOnOff.
	LazyRW string
	// WALRecycle "off" starts overlay branches with wal_recycle=off
	// ($PGOVERLAY_WAL_RECYCLE; "" = on; branchd --wal-recycle overrides it).
	// Experimental.
	WALRecycle string
}

// The environment variables behind Config.LazyRW and Config.WALRecycle, read
// by branchd (as its flags' defaults) and pgb in local mode alike.
const (
	LazyRWEnv     = "PGOVERLAY_LAZYRW"
	WALRecycleEnv = "PGOVERLAY_WAL_RECYCLE"
)

// ParseOnOff parses an on|off setting such as Config.LazyRW; "" gives def.
func ParseOnOff(s string, def bool) (bool, error) {
	switch s {
	case "":
		return def, nil
	case "on":
		return true, nil
	case "off":
		return false, nil
	}
	return false, fmt.Errorf("%q is not on or off", s)
}

func Load() (*Config, error) {
	home := os.Getenv("PGOVERLAY_HOME")
	if home == "" {
		uh, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = filepath.Join(uh, ".pgoverlay")
	}
	return &Config{
		Home:          home,
		RegistryPath:  filepath.Join(home, "pgoverlay.db"),
		PostgresImage: "postgres:17",
		LazyRW:        os.Getenv(LazyRWEnv),
		WALRecycle:    os.Getenv(WALRecycleEnv),
	}, nil
}

// EnsureHome creates the state directory owner-only (0700) and makes sure the
// registry file exists owner-only (0600) before SQLite opens it. The registry
// holds API-token digests, source connection details, masking SQL and branch
// passwords, and the directory holds the at-rest key, so neither may be
// readable by other local users. SQLite creates its -wal/-shm files with the
// database file's mode, so pre-creating the file 0600 covers them too.
//
// Existing installs created with the old 0755/0644 modes are tightened
// best-effort: a chmod the process is not allowed to make (a state dir owned
// by another user, some volume mounts) is not an error.
func (c *Config) EnsureHome() error {
	if err := os.MkdirAll(c.Home, 0o700); err != nil {
		return err
	}
	tighten(c.Home, 0o700)
	f, err := os.OpenFile(c.RegistryPath, os.O_RDONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		if err := f.Close(); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrExist):
		return fmt.Errorf("create registry file: %w", err)
	}
	for _, p := range []string{c.RegistryPath, c.RegistryPath + "-wal", c.RegistryPath + "-shm"} {
		tighten(p, 0o600)
	}
	return nil
}

// tighten drops any permission bits on path beyond max (best-effort; a
// missing file or a refused chmod is ignored).
func tighten(path string, max fs.FileMode) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if mode := fi.Mode().Perm(); mode&^max != 0 {
		_ = os.Chmod(path, mode&max)
	}
}
