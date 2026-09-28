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

// Seed runs pg_basebackup into the source volume. The helper runs as the
// in-image postgres user (uid 999) so file ownership matches branch
// containers. Data lands in <volume>/data because pg_basebackup insists on
// creating the target dir itself with 0700; the volume root is first chowned
// to uid 999 so it can. Requires REPLICATION privilege on the source
// (superuser works).
func Seed(ctx context.Context, d runtime.Driver, s SeedSpec) error {
	if err := s.Validate(); err != nil {
		return err
	}
	seedMount := runtime.Mount{Kind: s.MountKind, Volume: s.Volume, Target: "/seed"}
	if _, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image:  "alpine:3.21",
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
		return fmt.Errorf("pg_basebackup from %s: %w", s.addr(), err)
	}
	return nil
}
