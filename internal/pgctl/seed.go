// Package pgctl runs Postgres-side operations (seeding, readiness) through
// the runtime driver — pgbranch never touches data files from the host.
package pgctl

import (
	"context"
	"fmt"
	"strconv"

	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

type SeedSpec struct {
	Image    string // postgres image matching the source's major version
	Volume   string // target source volume
	Network  string // docker network from which the source is reachable ("" = bridge)
	Host     string
	Port     int
	User     string
	Password string
}

// Seed runs pg_basebackup into the source volume. The helper runs as the
// in-image postgres user (uid 999) so file ownership matches branch
// containers. Data lands in <volume>/data because pg_basebackup insists on
// creating the target dir itself with 0700; the volume root is first chowned
// to uid 999 so it can. Requires REPLICATION privilege on the source
// (superuser works).
func Seed(ctx context.Context, d runtime.Driver, s SeedSpec) error {
	if err := d.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
		Cmd:    []string{"sh", "-c", "mkdir -p /seed && chown 999:999 /seed"},
		Mounts: []runtime.Mount{{Volume: s.Volume, Target: "/seed"}},
	}); err != nil {
		return fmt.Errorf("prepare seed volume: %w", err)
	}
	err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: s.Image,
		User:  "postgres",
		Cmd: []string{"pg_basebackup",
			"-h", s.Host, "-p", strconv.Itoa(s.Port), "-U", s.User,
			"-D", "/seed/data", "-X", "stream", "--checkpoint=fast", "--no-password"},
		Env:     []string{"PGPASSWORD=" + s.Password},
		Mounts:  []runtime.Mount{{Volume: s.Volume, Target: "/seed"}},
		Network: s.Network,
	})
	if err != nil {
		return fmt.Errorf("pg_basebackup: %w", err)
	}
	return nil
}
