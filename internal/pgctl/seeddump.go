package pgctl

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// SeedDumpSpec seeds a source with pg_dump instead of pg_basebackup: a
// logical copy for managed Postgres (Supabase, Neon, RDS, Cloud SQL) where
// physical replication connections are not allowed.
type SeedDumpSpec struct {
	SeedSpec
	Database string   // remote database to dump ("" = postgres)
	Schemas  []string // schemas to dump (empty = the whole database)
}

// shellQuote single-quotes a value for safe embedding in the helper script.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// seedDumpScript is the helper script run by SeedDump (bash -c, in-image
// postgres user). It builds a fresh cluster in /seed/data and fills it from
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
//   - a temp socket-only server receives the dump; pg_dump streams from the
//     remote into psql with ON_ERROR_STOP (set -o pipefail makes a failing
//     pg_dump fail the pipe). When the schema scope explicitly names public,
//     pg_dump emits CREATE SCHEMA public — which already exists in a fresh
//     cluster — so it is dropped first (second placeholder); a whole-database
//     dump never emits it and the pre-created schema must stay.
//   - pg_ctl stop -m fast leaves a clean-shutdown cluster, so branches start
//     without crash recovery.
const seedDumpScript = `set -euo pipefail
initdb -D /seed/data --username="$PGB_USER" --pwfile=<(printf '%%s\n' "$PGB_PASSWORD") \
  --auth-local=trust --auth-host=scram-sha-256 --encoding=UTF8
echo "host all all all scram-sha-256" >> /seed/data/pg_hba.conf
echo "listen_addresses = '*'" >> /seed/data/postgresql.conf
pg_ctl -D /seed/data -o "-c listen_addresses='' -c unix_socket_directories=/tmp" -w start
if [ "$PGB_DB" != postgres ]; then createdb -h /tmp -U "$PGB_USER" "$PGB_DB"; fi
%sPGPASSWORD="$PGB_PASSWORD" pg_dump --no-owner --no-acl%s \
  -h "$PGB_REMOTE_HOST" -p "$PGB_REMOTE_PORT" -U "$PGB_USER" -d "$PGB_DB" \
  | psql -h /tmp -U "$PGB_USER" -d "$PGB_DB" -v ON_ERROR_STOP=1 -q
pg_ctl -D /seed/data -w stop -m fast
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
	dropPublic := ""
	for _, schema := range s.Schemas {
		schemaFlags.WriteString(" -n " + shellQuote(schema))
		if schema == "public" {
			dropPublic = `psql -h /tmp -U "$PGB_USER" -d "$PGB_DB" -q -c 'DROP SCHEMA public CASCADE'` + "\n"
		}
	}
	_, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: s.Image,
		User:  "postgres",
		Cmd:   []string{"bash", "-c", fmt.Sprintf(seedDumpScript, dropPublic, schemaFlags.String())},
		Env: []string{
			"PGB_USER=" + s.User,
			"PGB_PASSWORD=" + s.Password,
			"PGB_DB=" + db,
			"PGB_REMOTE_HOST=" + s.Host,
			"PGB_REMOTE_PORT=" + strconv.Itoa(s.Port),
		},
		Mounts:  []runtime.Mount{seedMount},
		Network: s.Network,
	})
	if err != nil {
		return fmt.Errorf("pg_dump seed: %w", err)
	}
	return nil
}
