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
	// VolumeRoot is a directory on the Docker host under which the docker
	// runtime creates every volume, as a bind volume, instead of letting
	// Docker store them ($PGOVERLAY_VOLUME_ROOT; branchd --volume-root
	// overrides it). "" = docker-managed volumes. See runtime.WithVolumeRoot.
	VolumeRoot string
}

// VolumeRootEnv names the environment variable that sets Config.VolumeRoot,
// for branchd and for pgb in local mode alike (both must agree: they share
// the registry and the volumes).
const VolumeRootEnv = "PGOVERLAY_VOLUME_ROOT"

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
		VolumeRoot:    os.Getenv(VolumeRootEnv),
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
