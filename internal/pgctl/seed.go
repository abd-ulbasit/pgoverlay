// Package pgctl runs Postgres-side operations (seeding, readiness) through
// the runtime driver — pgoverlay never touches data files from the host.
package pgctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// ErrSeedFailed marks a failure of the seed command itself (pg_basebackup, or
// the pg_dump | psql pipeline) as opposed to the runtime plumbing around it.
// Such a failure is almost always caused by the source's configuration (wrong
// password, no pg_hba entry, unreachable host, missing privilege) and its
// message carries the tool's own output, which never includes the password
// (it travels only in the helper's environment). Test with errors.Is.
var ErrSeedFailed = errors.New("seed command failed")

// seedError tags err with ErrSeedFailed without changing its message.
type seedError struct{ err error }

func (e seedError) Error() string   { return e.err.Error() }
func (e seedError) Unwrap() []error { return []error{ErrSeedFailed, e.err} }

type SeedSpec struct {
	Image string // postgres image matching the source's major version
	// Volume is the seed target: a volume name, or — with MountKind
	// MountHostPath (zfs backend) — the dataset's absolute mountpoint.
	Volume    string
	MountKind runtime.MountKind
	Network   string // docker network from which the source is reachable ("" = bridge)
	Host      string
	Port      int
	User      string
	Password  string
}

// Seed runs pg_basebackup into the source volume. The helper runs as the
// in-image postgres user (uid 999) so file ownership matches branch
// containers. Data lands in <volume>/data because pg_basebackup insists on
// creating the target dir itself with 0700; the volume root is first chowned
// to uid 999 so it can. Requires REPLICATION privilege on the source
// (superuser works).
func Seed(ctx context.Context, d runtime.Driver, s SeedSpec) error {
	seedMount := runtime.Mount{Kind: s.MountKind, Volume: s.Volume, Target: "/seed"}
	if _, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "mkdir -p /seed && chown 999:999 /seed"},
		Mounts: []runtime.Mount{seedMount},
	}); err != nil {
		return fmt.Errorf("prepare seed volume: %w", err)
	}
	_, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: s.Image,
		User:  "postgres",
		Cmd: []string{"pg_basebackup",
			"-h", s.Host, "-p", strconv.Itoa(s.Port), "-U", s.User,
			"-D", "/seed/data", "-X", "stream", "--checkpoint=fast", "--no-password"},
		Env:     []string{"PGPASSWORD=" + s.Password},
		Mounts:  []runtime.Mount{seedMount},
		Network: s.Network,
	})
	if err != nil {
		return seedError{fmt.Errorf("pg_basebackup: %w", err)}
	}
	return nil
}
