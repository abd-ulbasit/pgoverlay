// Package pgctl runs Postgres-side operations (seeding, readiness) through
// the runtime driver — pgoverlay never touches data files from the host.
package pgctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// ErrInvalidSpec marks a seed request that cannot succeed as given: no host,
// a port out of range, an unknown sslmode. Callers reject it before any
// state is created (the API answers 400).
var ErrInvalidSpec = errors.New("invalid seed spec")

// ConnectTimeout bounds every libpq connection attempt the seed helpers make
// to the source (PGCONNECT_TIMEOUT), so a wrong address or a firewall that
// drops packets fails in seconds instead of hanging until the OS gives up.
const ConnectTimeout = 10 * time.Second

// SSLModeEnv is the environment variable (of branchd, or of pgb without
// --server) that sets the libpq sslmode for source connections when a
// SeedSpec leaves SSLMode empty. DefaultSSLMode applies when it is unset.
const SSLModeEnv = "PGOVERLAY_SEED_SSLMODE"

// DefaultSSLMode is libpq's own default: TLS when the server offers it,
// without certificate verification, falling back to plaintext. Managed
// providers reached over the internet warrant require or verify-full.
const DefaultSSLMode = "prefer"

// sslModes are the libpq sslmode values.
var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

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
	// SSLMode is the libpq sslmode for the source connection ("" =
	// $PGOVERLAY_SEED_SSLMODE, else DefaultSSLMode).
	SSLMode string
	// Settle is how the seeded cluster is prepared before any branch starts
	// from it ("" = DefaultSettleMode). Seed leaves the cluster as
	// pg_basebackup wrote it and the caller runs Settle next; SeedDump
	// applies the mode inside its own helper.
	Settle SettleMode
}

// sslMode resolves the effective sslmode.
func (s SeedSpec) sslMode() string {
	if s.SSLMode != "" {
		return s.SSLMode
	}
	if m := os.Getenv(SSLModeEnv); m != "" {
		return m
	}
	return DefaultSSLMode
}

// Validate rejects connection settings that can never seed. An empty host is
// the important one: libpq would silently fall back to a Unix socket inside
// the helper container, where no server runs, and fail with an error that
// does not mention the host at all.
func (s SeedSpec) Validate() error {
	host := strings.TrimSpace(s.Host)
	switch {
	case host == "":
		return fmt.Errorf("%w: source host is empty", ErrInvalidSpec)
	case strings.HasPrefix(host, "/"):
		return fmt.Errorf("%w: source host %q is a Unix socket directory, which the seed helper container cannot reach; use an address reachable from containers", ErrInvalidSpec, s.Host)
	case s.Port < 1 || s.Port > 65535:
		return fmt.Errorf("%w: source port %d is out of range 1-65535", ErrInvalidSpec, s.Port)
	case strings.TrimSpace(s.User) == "":
		return fmt.Errorf("%w: source user is empty", ErrInvalidSpec)
	}
	if m := s.sslMode(); !slices.Contains(sslModes, m) {
		return fmt.Errorf("%w: sslmode %q (from the seed request or $%s): want one of %s",
			ErrInvalidSpec, m, SSLModeEnv, strings.Join(sslModes, ", "))
	}
	if _, err := ParseSettleMode(string(s.Settle)); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	return nil
}

// addr is host:port for messages.
func (s SeedSpec) addr() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// connEnv is the libpq environment for a helper process that connects to the
// source: the TLS mode and a bounded connect time.
func (s SeedSpec) connEnv() []string {
	return []string{
		"PGSSLMODE=" + s.sslMode(),
		"PGCONNECT_TIMEOUT=" + strconv.Itoa(int(ConnectTimeout/time.Second)),
	}
}

// ErrVersionMismatch reports a seeded cluster whose major version differs
// from the branch image's: Postgres refuses to start on it, so every branch
// would fail.
var ErrVersionMismatch = errors.New("source PostgreSQL major version does not match the branch image")

// basebackupFixupScript runs (sh -c, in the branch image, as postgres) on
// the data dir pg_basebackup produced ($1). pg_basebackup copies a standby's
// standby.signal and postgresql.auto.conf verbatim, so a base backup of a
// streaming replica would boot every branch as a read-only hot standby that
// replicates from production with production's credentials. The script
// deletes the recovery signal files and removes the recovery and replication
// settings from the config files (the whole line: primary_conninfo usually
// carries a password), then reports what it did plus the cluster's and the
// image's major versions as key=value lines. It never prints a config line.
const basebackupFixupScript = `set -eu
d=$1
for f in standby.signal recovery.signal; do
  if [ -e "$d/$f" ]; then
    rm -f "$d/$f"
    echo "pgoverlay-removed=$f"
  fi
done
re='^[[:space:]]*(primary_conninfo|primary_slot_name|restore_command|archive_cleanup_command|recovery_end_command|recovery_min_apply_delay|promote_trigger_file|recovery_target[a-z_]*)([[:space:]]|=|$)'
for c in postgresql.auto.conf postgresql.conf; do
  f="$d/$c"
  [ -f "$f" ] || continue
  grep -Eiq "$re" "$f" || continue
  grep -Eiv "$re" "$f" > "$f.pgoverlay" || [ $? -eq 1 ]
  cat "$f.pgoverlay" > "$f"
  rm -f "$f.pgoverlay"
  echo "pgoverlay-stripped=$c"
done
echo "pgoverlay-data-version=$(cat "$d/PG_VERSION")"
echo "pgoverlay-server-version=$(postgres -V 2>/dev/null)"
`

// seededCluster is what basebackupFixupScript reports.
type seededCluster struct {
	dataMajor   string   // PG_VERSION of the copied cluster
	serverMajor string   // major version of the branch image's postgres binary
	removed     []string // recovery signal files deleted
	stripped    []string // config files recovery settings were removed from
}

var serverMajorRe = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

func parseSeededCluster(out string) seededCluster {
	var c seededCluster
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "pgoverlay-data-version":
			c.dataMajor = strings.TrimSpace(v)
		case "pgoverlay-server-version":
			if m := serverMajorRe.FindStringSubmatch(v); m != nil {
				c.serverMajor = m[1]
			}
		case "pgoverlay-removed":
			c.removed = append(c.removed, v)
		case "pgoverlay-stripped":
			c.stripped = append(c.stripped, v)
		}
	}
	return c
}

// Seed runs pg_basebackup into the source volume. The helper runs as the
// in-image postgres user (uid 999) so file ownership matches branch
// containers. Data lands in <volume>/data because pg_basebackup insists on
// creating the target dir itself with 0700; the volume root is first chowned
// to uid 999 so it can. Requires REPLICATION privilege on the source
// (superuser works).
//
// A standby works as a source: its recovery state is removed from the copy
// (basebackupFixupScript) so branches start as independent, writable
// primaries. The copy's major version must match the image's, which is
// checked here rather than 90 seconds into every branch's readiness wait.
func Seed(ctx context.Context, d runtime.Driver, s SeedSpec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	seedMount := runtime.Mount{Kind: s.MountKind, Volume: s.Volume, Target: "/seed"}
	if _, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image:  runtime.UtilityImage,
		Cmd:    []string{"sh", "-c", "mkdir -p /seed && chown 999:999 /seed"},
		Mounts: []runtime.Mount{seedMount},
	}); err != nil {
		return fmt.Errorf("prepare seed volume: %w", err)
	}
	slog.Info("seed: running pg_basebackup against the source", "addr", s.addr(), "user", s.User, "sslmode", s.sslMode())
	_, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: s.Image,
		User:  "postgres",
		Cmd: []string{"pg_basebackup",
			"-h", s.Host, "-p", strconv.Itoa(s.Port), "-U", s.User,
			"-D", "/seed/data", "-X", "stream", "--checkpoint=fast", "--no-password"},
		Env:     append([]string{"PGPASSWORD=" + s.Password}, s.connEnv()...),
		Mounts:  []runtime.Mount{seedMount},
		Network: s.Network,
	})
	if err != nil {
		return seedError{fmt.Errorf("pg_basebackup from %s: %w", s.addr(), err)}
	}
	out, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image:  s.Image,
		User:   "postgres",
		Cmd:    []string{"sh", "-c", basebackupFixupScript, "pgoverlay-seed-fixup", "/seed/data"},
		Mounts: []runtime.Mount{seedMount},
	})
	if err != nil {
		return fmt.Errorf("prepare seeded data dir: %w", err)
	}
	c := parseSeededCluster(out)
	if len(c.removed) > 0 || len(c.stripped) > 0 {
		slog.Warn("seed: the source's base backup carried standby/recovery state; removed it so branches start as writable primaries that do not replicate from the source",
			"addr", s.addr(), "removed_files", c.removed, "stripped_settings_from", c.stripped)
	}
	switch {
	case c.dataMajor == "" || c.serverMajor == "":
		slog.Warn("seed: could not compare the source's major version with the branch image's", "image", s.Image, "output", out)
	case c.dataMajor != c.serverMajor:
		return fmt.Errorf("%w: the source at %s is PostgreSQL %s but the branch image %s runs PostgreSQL %s; add the source with pg_version %s (pgb source add --pg-version %s)",
			ErrVersionMismatch, s.addr(), c.dataMajor, s.Image, c.serverMajor, c.dataMajor, c.dataMajor)
	}
	return nil
}
