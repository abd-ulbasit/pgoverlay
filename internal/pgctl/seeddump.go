package pgctl

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// SeedDumpSpec seeds a source with pg_dump instead of pg_basebackup: a
// logical copy for managed Postgres (Supabase, Neon, RDS, Cloud SQL) where
// physical replication connections are not allowed.
type SeedDumpSpec struct {
	SeedSpec
	Database string   // remote database to dump ("" = postgres)
	Schemas  []string // schemas to dump (empty = the whole database)
}

// Validate checks the connection settings and the schema patterns. A pattern
// must not contain a comma: the registry stores the list comma-joined, so a
// refresh would split it into two patterns.
func (s SeedDumpSpec) Validate() error {
	if err := s.SeedSpec.Validate(); err != nil {
		return err
	}
	for _, schema := range s.Schemas {
		if strings.TrimSpace(schema) == "" {
			return fmt.Errorf("%w: empty dump schema pattern", ErrInvalidSpec)
		}
		if strings.Contains(schema, ",") {
			return fmt.Errorf("%w: dump schema pattern %q contains a comma, which cannot be stored; match that character with the ? wildcard outside double quotes", ErrInvalidSpec, schema)
		}
	}
	return nil
}

// shellQuote single-quotes a value for safe embedding in the helper script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// seedDumpScript is the helper script run by SeedDump (bash -c, in-image
// postgres user). It builds a fresh cluster in $PGB_DATA and fills it from
// the remote database:
//
//   - initdb with the same user/password the caller registered the source
//     with, so branch containers accept the same credentials as basebackup
//     mode. --auth-local=trust (matching the stock docker image's bootstrap
//     behavior) lets the local pipe leg connect over the socket without a
//     password; --auth-host=scram-sha-256 keeps TCP locked down. The password
//     reaches initdb via a bash process substitution pwfile, never argv.
//   - basebackup-cloned sources inherit listen_addresses='*' and a permissive
//     pg_hba.conf from production; a fresh initdb has neither, so they are
//     appended here — branch containers start this PGDATA directly.
//   - a temp socket-only server receives the dump. Its log goes to a file:
//     the server log repeats failing statements and COPY rows, and the
//     helper's output ends up in error messages and the registry.
//   - --no-owner/--no-acl drop ownership and grants, but row-level security
//     policies still name roles (Supabase: TO authenticated). Every
//     non-system role of the source is recreated first as a NOLOGIN shell
//     (pg_roles is readable by any user).
//   - pg_dump -n dumps no extensions, yet the dumped tables and defaults use
//     their types and functions (citext columns, uuid_generate_v4()). For a
//     scoped dump each of the source's extensions is created first, in its
//     own schema; one this image lacks is reported and skipped.
//   - pg_dump streams from the remote into psql with ON_ERROR_STOP (set -o
//     pipefail makes a failing pg_dump fail the pipe). A scoped dump emits
//     CREATE SCHEMA for every schema it selects — whatever the -n pattern
//     looks like (public, PUBLIC, pub*) — and public and the extension
//     schemas already exist, so those statements are made idempotent. Only
//     lines before the first COPY are rewritten: schemas are pre-data, and
//     row data never passes through the expression.
//   - psql reports errors tersely (no DETAIL/CONTEXT lines, which quote
//     whole rows) and redact masks the one value a data error still quotes.
//   - pg_ctl stop -m fast leaves a clean-shutdown cluster, so branches start
//     without crash recovery.
//
// The placeholder is the pg_dump -n flags.
const seedDumpScript = `set -euo pipefail
remote() {
  PGPASSWORD="$PGB_PASSWORD" PGSSLMODE="$PGB_SSLMODE" PGCONNECT_TIMEOUT="$PGB_CONNECT_TIMEOUT" \
    "$@" -h "$PGB_REMOTE_HOST" -p "$PGB_REMOTE_PORT" -U "$PGB_USER" -d "$PGB_DB"
}
temp_psql() {
  psql -X -q -h /tmp -U "$PGB_USER" -d "$PGB_DB" -v VERBOSITY=terse "$@"
}
schema_filter() {
  if [ -n "${PGB_SCOPED:-}" ]; then
    sed -e '/^COPY .* FROM stdin;$/,$!s/^CREATE SCHEMA \(.*\);$/CREATE SCHEMA IF NOT EXISTS \1;/'
  else
    cat
  fi
}
redact() {
  sed -e '/^psql:/!s/.*/[redacted]/' -e 's/\(ERROR: .*: \)".*/\1"[redacted]"/'
}
initdb -D "$PGB_DATA" --username="$PGB_USER" --pwfile=<(printf '%%s\n' "$PGB_PASSWORD") \
  --auth-local=trust --auth-host=scram-sha-256 --encoding=UTF8 >/dev/null
echo "host all all all scram-sha-256" >> "$PGB_DATA/pg_hba.conf"
echo "listen_addresses = '*'" >> "$PGB_DATA/postgresql.conf"
pg_ctl -D "$PGB_DATA" -l /tmp/pgoverlay-seed.log \
  -o "-c listen_addresses='' -c unix_socket_directories=/tmp" -w start >/dev/null \
  || { tail -n 20 /tmp/pgoverlay-seed.log >&2; exit 1; }
if [ "$PGB_DB" != postgres ]; then createdb -h /tmp -U "$PGB_USER" "$PGB_DB"; fi
{ echo 'SET client_min_messages = warning;'
  remote psql -X -A -t -v ON_ERROR_STOP=1 -c "SELECT 'CREATE ROLE ' || quote_ident(rolname) || ' NOLOGIN;' FROM pg_roles WHERE rolname !~ '^pg_' AND rolname <> current_user ORDER BY oid"
  if [ -n "${PGB_SCOPED:-}" ]; then
    remote psql -X -A -t -v ON_ERROR_STOP=1 -c "SELECT 'CREATE SCHEMA IF NOT EXISTS ' || quote_ident(n.nspname) || '; CREATE EXTENSION IF NOT EXISTS ' || quote_ident(e.extname) || ' WITH SCHEMA ' || quote_ident(n.nspname) || ' CASCADE;' FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname <> 'plpgsql' ORDER BY e.oid"
  fi
} | temp_psql >/dev/null
remote pg_dump --no-owner --no-acl%s \
  | schema_filter \
  | temp_psql -v ON_ERROR_STOP=1 -v SHOW_CONTEXT=never 2>&1 >/dev/null \
  | redact >&2
pg_ctl -D "$PGB_DATA" -w stop -m fast >/dev/null
`

// SeedDump builds the source volume from a logical dump: initdb a fresh
// cluster, then pg_dump | psql from the remote — all inside one helper
// container running as the in-image postgres user (uid 999), so file
// ownership matches branch containers. Unlike Seed it needs only a normal
// user on the remote (no REPLICATION privilege), which makes managed
// providers usable as sources. The helper image's major version must be >=
// the remote server's (pg_dump cannot dump newer servers) and branches run
// the cluster initdb produced, i.e. the helper image's version.
func SeedDump(ctx context.Context, d runtime.Driver, s SeedDumpSpec) error {
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
	db := s.Database
	if db == "" {
		db = "postgres"
	}
	var schemaFlags strings.Builder
	for _, schema := range s.Schemas {
		schemaFlags.WriteString(" -n " + shellQuote(schema))
	}
	scoped := ""
	if len(s.Schemas) > 0 {
		scoped = "1"
	}
	slog.Info("seed: running pg_dump against the source", "addr", s.addr(), "user", s.User, "database", db, "sslmode", s.sslMode())
	_, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: s.Image,
		User:  "postgres",
		Cmd:   []string{"bash", "-c", fmt.Sprintf(seedDumpScript, schemaFlags.String())},
		Env: []string{
			"PGB_DATA=/seed/data",
			"PGB_USER=" + s.User,
			"PGB_PASSWORD=" + s.Password,
			"PGB_DB=" + db,
			"PGB_SCOPED=" + scoped,
			"PGB_REMOTE_HOST=" + s.Host,
			"PGB_REMOTE_PORT=" + strconv.Itoa(s.Port),
			// libpq settings for the remote leg only (the local socket leg
			// needs neither)
			"PGB_SSLMODE=" + s.sslMode(),
			"PGB_CONNECT_TIMEOUT=" + strconv.Itoa(int(ConnectTimeout/time.Second)),
		},
		Mounts:  []runtime.Mount{seedMount},
		Network: s.Network,
	})
	if err != nil {
		return fmt.Errorf("pg_dump seed from %s: %w", s.addr(), err)
	}
	return nil
}
