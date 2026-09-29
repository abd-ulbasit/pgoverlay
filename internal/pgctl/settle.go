package pgctl

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

// SettleMode is how a freshly seeded cluster is prepared before any branch
// starts from it (branchd --seed-settle, pgb $PGOVERLAY_SEED_SETTLE).
//
// A pg_basebackup copy is an online backup: every branch that starts from it
// replays the WAL streamed during the backup, and the copied pages carry
// whatever hint bits, dead tuples and unfrozen xids the source had. Each of
// those turns a read in a branch into a write (backup-label replay, hint-bit
// setting, HOT pruning, anti-wraparound autovacuum), and on the overlay
// backend a write copies the touched file into the branch. Settling does that
// work once, in the seed, so branches start from a clean shutdown and their
// reads write nothing.
type SettleMode string

const (
	// SettleFreeze recovers the seed, runs VACUUM (FREEZE, ANALYZE) on every
	// database and shuts it down cleanly. The default.
	SettleFreeze SettleMode = "freeze"
	// SettleRecover recovers the seed and shuts it down cleanly, without
	// the VACUUM: branches skip WAL replay but may still set hint bits.
	SettleRecover SettleMode = "recover"
	// SettleOff leaves the seed exactly as the seed command wrote it (the
	// behaviour before settling existed).
	SettleOff SettleMode = "off"
)

// DefaultSettleMode applies when a SeedSpec leaves Settle empty.
const DefaultSettleMode = SettleFreeze

// SettleEnv is the environment variable that sets the settle mode: pgb reads
// it in local mode, branchd uses it as the --seed-settle default.
const SettleEnv = "PGOVERLAY_SEED_SETTLE"

var settleModes = []SettleMode{SettleFreeze, SettleRecover, SettleOff}

// ParseSettleMode validates a settle mode; "" is DefaultSettleMode.
func ParseSettleMode(s string) (SettleMode, error) {
	m := SettleMode(strings.ToLower(strings.TrimSpace(s)))
	if m == "" {
		return DefaultSettleMode, nil
	}
	if !slices.Contains(settleModes, m) {
		return "", fmt.Errorf("unknown seed settle mode %q (want freeze, recover or off)", s)
	}
	return m, nil
}

// settleWaitSeconds bounds pg_ctl's own waits (PGCTLTIMEOUT: the
// backup-label recovery on start, the shutdown checkpoint on stop). Its
// default of 60 s is far too short for a large backup; the helper's context is
// what really bounds them.
const settleWaitSeconds = 86400

// walTrimFunction defines the shell function trim_wal DATADIR, which the
// settle and dump scripts run on a cleanly shut down seed. It sets $wal to
// "trimmed" when it made the all-zero tail of the WAL segment that holds the
// latest checkpoint a hole, and leaves it alone otherwise. It also removes
// the segments of the checkpoint's timeline that come after that one, and
// counts them in $walremoved.
//
// Those later segments are preallocated or recycled ahead of time by
// checkpoints (a dump seed's restore leaves up to max_wal_size of them) and
// hold no WAL of this cluster: after a clean shutdown nothing follows the
// checkpoint record. Postgres creates a missing segment when it gets there,
// so removing them only makes the seed smaller (and spares each branch a
// copy-up of stale bytes when its WAL reaches them).
//
// A branch starts writing WAL right after the seed's shutdown checkpoint, in
// that same segment, so on the overlay backend its first WAL write copies the
// whole segment (16 MiB by default) up into the branch: about 50 ms and 16 MiB
// of disk per branch on ext4, measured. The scripts therefore switch to a
// fresh segment just before the clean stop, which puts the shutdown
// checkpoint at the start of an otherwise zero-filled segment (the settle
// server runs with wal_recycle=off, so a new segment is created zero-filled,
// never recycled), and trim_wal turns everything after the checkpoint
// record's page and the one after it into a hole. OverlayFS copies holes up
// as holes (it skips them with SEEK_DATA), so the first WAL write in a
// branch then copies a few KiB instead of the segment.
//
// The file stays byte-for-byte identical: the tail is only replaced when it
// reads as zeros, the trimmed copy is written next to it, compared with cmp
// and renamed over the original. A hole reads as zeros, which Postgres treats
// as the end of WAL exactly as it does zero-filled pages; the only difference
// is that the blocks are allocated when the branch writes them instead of in
// advance, for this one segment (wal_init_zero keeps preallocating every
// segment the branch creates itself). Anything unexpected (an unreadable
// control file, a non-zero tail, a failed copy) keeps the segment as it is.
// It uses dd, truncate, tr and cmp, which both the Debian and the Alpine
// postgres images carry, and no '%' (the dump script is a format string).
const walTrimFunction = `pg_field() {
  LC_ALL=C pg_controldata "$1" 2>/dev/null | sed -n "s/^$2: *//p"
}
trim_wal() {
  seg=$(pg_field "$1" "Latest checkpoint's REDO WAL file") || return 1
  loc=$(pg_field "$1" "Latest checkpoint's REDO location") || return 1
  page=$(pg_field "$1" "WAL block size") || return 1
  case $seg in '' | *[!0-9A-F]*) return 1 ;; esac
  tli=$(expr "$seg" : '\([0-9A-F]\{8\}\)[0-9A-F]\{16\}$') || return 1
  after=
  for name in $(ls "$1/pg_wal" | grep -E "^$tli[0-9A-F]{16}\$" | LC_ALL=C sort); do
    if [ -n "$after" ]; then
      rm -f "$1/pg_wal/$name" && walremoved=$(( walremoved + 1 ))
    fi
    if [ "$name" = "$seg" ]; then after=1; fi
  done
  case $page in '' | 0 | *[!0-9]*) return 1 ;; esac
  case $loc in */*) ;; *) return 1 ;; esac
  lo=${loc#*/}
  case $lo in '' | *[!0-9A-Fa-f]*) return 1 ;; esac
  f=$1/pg_wal/$seg
  [ -f "$f" ] || return 1
  size=$(wc -c < "$f" | tr -d ' ') || return 1
  case $size in '' | 0 | *[!0-9]*) return 1 ;; esac
  off=$(( 0x$lo - 0x$lo / size * size ))
  cut=$(( (off / page + 2) * page ))
  [ "$cut" -lt "$size" ] || return 1
  nonzero=$(dd if="$f" bs="$page" skip=$(( cut / page )) 2>/dev/null | tr -d '\000' | wc -c | tr -d ' ') || return 1
  [ "$nonzero" = 0 ] || return 1
  tmp=$f.pgoverlay-trim
  if dd if="$f" of="$tmp" bs="$page" count=$(( cut / page )) 2>/dev/null &&
    truncate -s "$size" "$tmp" && chmod 0600 "$tmp" && cmp -s "$f" "$tmp" && mv -f "$tmp" "$f"; then
    wal=trimmed
  else
    rm -f "$tmp"
    return 1
  fi
}
`

// settleScript runs (sh -c, in the branch image, as postgres) on the seeded
// data dir ($1). $PGB_SETTLE is recover or freeze, $PGB_USER the role the
// source was seeded as, and $PGCTLTIMEOUT bounds pg_ctl's waits.
//
// It starts postgres once on the seed with overrides that make any
// production postgresql.conf safe to start in a throwaway container:
//
//   - no TCP listener, a private socket directory, and a temporary
//     "local all all trust" hba and empty ident file (the source's may live
//     outside the data dir, or demand passwords this helper does not have).
//     Only processes in this container can reach the socket.
//   - no TLS (certificates outside the data dir), no logging collector (log
//     directories outside it), no WAL archiving, no synchronous standby wait,
//     no preload libraries (a library the image lacks would stop the start,
//     and background workers such as pg_cron or a TimescaleDB job scheduler
//     must not run against the seed), no login event triggers (17+), and
//     io_method=worker (18+; io_uring is blocked by the default seccomp).
//   - small, fixed memory: shared_buffers=128MB, no huge pages, no
//     preallocated dynamic shared memory; autovacuum off (the VACUUM below
//     does its work).
//   - fsync, full_page_writes and synchronous_commit off, which makes the
//     VACUUM and the recovery several times cheaper. That is safe here
//     because a settle that does not finish is never used: any failure fails
//     the seed and the engine removes the half-settled layer; after the clean
//     stop the script syncs the filesystem before it reports success. The
//     shutdown checkpoint then records full_page_writes=off, but that only
//     seeds shared memory: a branch applies its own setting at startup,
//     before it can write anything, so its crash safety is unchanged.
//   - wal_recycle off and wal_keep_size 0, so the settled seed keeps no
//     recycled WAL segments: branches do not inherit (and on the overlay
//     backend copy up) gigabytes of preallocated WAL.
//   - a missing postgresql.conf (distro-packaged sources keep it in /etc) is
//     replaced by an empty one, as the branch entrypoint does.
//
// The start completes the base backup's recovery (backup_label becomes
// backup_label.old). In freeze mode vacuumdb then runs VACUUM (FREEZE,
// ANALYZE) on every database as the first superuser that can log in, with
// the session timeouts and a read-only default that per-role or
// per-database settings could impose turned off, and without parallel
// workers (their dynamic shared memory lives in /dev/shm, 64 MB in Docker).
// A VACUUM failure is reported, not fatal: the seed is still recovered and
// cleanly shut down, which is what branches need to start; reads in them may
// just copy more.
//
// vacuumdb analyzes the tables it reaches after pg_statistic (and
// pg_statistic_ext_data) have been vacuumed, so their new statistics rows
// would be the only unfrozen, unhinted tuples left: the first query planned
// in every branch would set hint bits on them and copy the catalog up. A
// VACUUM (FREEZE) of those two catalogs in every database that accepts
// connections follows vacuumdb (whether it succeeded or not); a database is
// named through PGDATABASE, which libpq never parses as a connection string.
// A failure there is reported (pgoverlay-settle-statistics=failed), not
// fatal. A CHECKPOINT, a switch to a fresh WAL segment and a fast shutdown
// follow, and pg_controldata must then report "shut down". trim_wal (see
// walTrimFunction) then makes the rest of the checkpoint's segment a hole
// (pgoverlay-settle-wal=trimmed or kept).
//
// The last lines of output are key=value reports (the runtime keeps only the
// tail of a helper's output). A vacuumdb error keeps its first part but not
// what follows the last ": " before a quote, which can be a row value (an
// ANALYZE evaluating an expression index); the server log tail printed on a
// failed start or stop is filtered the same way, without the DETAIL, CONTEXT
// and STATEMENT lines that quote rows.
const settleScript = "set -eu\n" + walTrimFunction + `logtail() {
  grep -v -e 'DETAIL:' -e 'CONTEXT:' -e 'STATEMENT:' "$log" | tail -n 15 \
    | sed -e 's/\(ERROR: .*: \)".*/\1"[redacted]"/' >&2 || true
}
d=$1
mode=${PGB_SETTLE:-freeze}
major=$(cat "$d/PG_VERSION")
freeze_statistics='VACUUM (FREEZE) pg_catalog.pg_statistic, pg_catalog.pg_statistic_ext_data'
t=$(mktemp -d "${TMPDIR:-/tmp}/pgoverlay-settle.XXXXXX")
log=$t/server.log
printf 'local all all trust\n' > "$t/pg_hba.conf"
: > "$t/pg_ident.conf"
o="-c data_directory=$d -c hba_file=$t/pg_hba.conf -c ident_file=$t/pg_ident.conf"
if [ ! -e "$d/postgresql.conf" ]; then
  : > "$t/postgresql.conf"
  o="$o -c config_file=$t/postgresql.conf"
fi
o="$o -c listen_addresses= -c port=5432 -c unix_socket_directories=$t -c unix_socket_permissions=0700 -c ssl=off"
o="$o -c logging_collector=off -c log_destination=stderr -c external_pid_file="
o="$o -c archive_mode=off -c synchronous_standby_names= -c wal_keep_size=0 -c wal_recycle=off"
o="$o -c shared_preload_libraries= -c session_preload_libraries= -c local_preload_libraries="
o="$o -c shared_buffers=128MB -c huge_pages=off -c min_dynamic_shared_memory=0 -c autovacuum=off"
o="$o -c fsync=off -c full_page_writes=off -c synchronous_commit=off -c recovery_init_sync_method=syncfs"
if [ "$major" -ge 17 ]; then o="$o -c event_triggers=off"; fi
if [ "$major" -ge 18 ]; then o="$o -c io_method=worker"; fi
if ! pg_ctl -D "$d" -l "$log" -o "$o" -w start >/dev/null 2>&1; then
  echo "postgres did not start on the seeded data directory; its log ends:" >&2
  logtail
  exit 1
fi
export PGHOST="$t" PGPORT=5432 PGAPPNAME=pgoverlay-settle
PGOPTIONS='-c statement_timeout=0 -c lock_timeout=0 -c idle_in_transaction_session_timeout=0 -c default_transaction_read_only=off -c max_parallel_maintenance_workers=0'
if [ "$major" -ge 17 ]; then PGOPTIONS="$PGOPTIONS -c transaction_timeout=0"; fi
export PGOPTIONS
su=
sudb=
for db in template1 postgres; do
  for u in ${PGB_USER:+"$PGB_USER"} postgres; do
    su=$(psql -X -A -t -U "$u" -d "$db" -c "SELECT rolname FROM pg_roles WHERE rolsuper AND rolcanlogin ORDER BY oid <> 10, oid LIMIT 1" 2>/dev/null) || su=
    if [ -n "$su" ]; then sudb=$db; break 2; fi
  done
done
vacuum=skipped
if [ "$mode" = freeze ]; then
  if [ -z "$su" ]; then
    vacuum=no-superuser
  elif vacuumdb -U "$su" --all --freeze --analyze --skip-locked >/dev/null 2>"$t/vacuum.err"; then
    vacuum=ok
  else
    vacuum=failed
  fi
fi
locked=$(grep -c 'lock not available' "$t/vacuum.err" 2>/dev/null) || locked=0
statistics=skipped
if [ "$vacuum" = ok ] || [ "$vacuum" = failed ]; then
  statistics=ok
  dbs=$(psql -X -A -t -U "$su" -d "$sudb" -c "SELECT datname FROM pg_database WHERE datallowconn ORDER BY datname" 2>/dev/null) || { dbs=; statistics=failed; }
  while IFS= read -r db; do
    [ -n "$db" ] || continue
    PGDATABASE=$db psql -X -q -U "$su" -c "$freeze_statistics" >/dev/null 2>&1 || statistics=failed
  done <<EOF
$dbs
EOF
fi
if [ -n "$su" ]; then
  psql -X -q -U "$su" -d "$sudb" -c CHECKPOINT >/dev/null 2>&1 || true
  psql -X -q -U "$su" -d "$sudb" -c 'SELECT pg_switch_wal()' >/dev/null 2>&1 || true
fi
if ! pg_ctl -D "$d" -m fast -w stop >/dev/null 2>&1; then
  echo "postgres did not stop cleanly on the seeded data directory; its log ends:" >&2
  logtail
  exit 1
fi
state=$(LC_ALL=C pg_controldata "$d" | sed -n 's/^Database cluster state: *//p')
if [ "$state" != "shut down" ]; then
  echo "the seeded cluster is not cleanly shut down after settling (pg_controldata: ${state:-unreadable})" >&2
  exit 1
fi
rm -f "$d/postmaster.opts"
wal=kept walremoved=0
trim_wal "$d" || :
sync -f "$d" 2>/dev/null || sync
if [ "$vacuum" = failed ]; then
  grep '^vacuumdb:' "$t/vacuum.err" | tail -n 3 | sed -e 's/\(ERROR: .*: \)".*/\1"[redacted]"/' -e 's/^/pgoverlay-settle-vacuum-error=/'
fi
rm -rf "$t"
echo "pgoverlay-settle-superuser=$su"
echo "pgoverlay-settle-locked=$locked"
echo "pgoverlay-settle-statistics=$statistics"
echo "pgoverlay-settle-wal=$wal"
echo "pgoverlay-settle-wal-removed=$walremoved"
echo "pgoverlay-settle-vacuum=$vacuum"
echo "pgoverlay-settle-state=$state"
`

// settleReport is what settleScript (and the dump script's VACUUM step)
// report.
type settleReport struct {
	vacuum string // ok, failed, no-superuser, skipped ("" = not reported)
	// statistics is the outcome of the VACUUM (FREEZE) of the statistics
	// catalogs in every database after vacuumdb: ok, failed or skipped
	// ("" = not reported).
	statistics string
	// wal says whether the zero tail of the checkpoint's WAL segment was
	// made a hole (trimmed) or left as it was (kept); see walTrimFunction.
	wal string
	// walRemoved counts the unused WAL segments after the checkpoint's
	// that were removed from the seed.
	walRemoved int
	superuser  string   // role the VACUUM ran as
	locked     int      // relations VACUUM skipped because another session held a lock
	state      string   // pg_control cluster state after the stop
	errors     []string // vacuumdb error lines, with values redacted
}

func parseSettleReport(out string) settleReport {
	var r settleReport
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "pgoverlay-settle-vacuum":
			r.vacuum = v
		case "pgoverlay-settle-superuser":
			r.superuser = v
		case "pgoverlay-settle-statistics":
			r.statistics = v
		case "pgoverlay-settle-wal":
			r.wal = v
		case "pgoverlay-settle-wal-removed":
			r.walRemoved, _ = strconv.Atoi(v)
		case "pgoverlay-settle-locked":
			r.locked, _ = strconv.Atoi(v)
		case "pgoverlay-settle-state":
			r.state = v
		case "pgoverlay-settle-vacuum-error":
			r.errors = append(r.errors, v)
		}
	}
	return r
}

// log reports the outcome of a settle (attrs identify the seed). A VACUUM
// that failed or could not run is a warning, not an error: the seed is still
// cleanly shut down, which is all branches need to start.
func (r settleReport) log(attrs ...any) {
	if r.statistics == "failed" {
		slog.Warn("seed settle: the final VACUUM (FREEZE) of pg_statistic failed in at least one database; the first query planned in a branch may set hint bits on it and copy it into the branch",
			attrs...)
	}
	if r.wal != "" {
		// trimmed: a branch's first WAL write copies a few KiB, not a segment
		attrs = append(attrs, "wal_tail", r.wal, "wal_segments_removed", r.walRemoved)
	}
	switch r.vacuum {
	case "ok":
		if r.locked > 0 {
			slog.Warn("seed settle: VACUUM (FREEZE, ANALYZE) skipped relations locked by another session (a prepared transaction copied from the source); reads of them in branches may copy data",
				append(attrs, "skipped_relations", r.locked)...)
		}
		if r.superuser != "" {
			attrs = append(attrs, "superuser", r.superuser)
		}
		slog.Info("seed settle: the seed is frozen, analyzed and cleanly shut down", attrs...)
	case "failed":
		slog.Warn("seed settle: VACUUM (FREEZE, ANALYZE) failed; the seed is still cleanly shut down, so branches start without crash recovery, but reads in them may set hint bits and copy data",
			append(attrs, "error", strings.Join(r.errors, "; "))...)
	case "no-superuser":
		slog.Warn("seed settle: no role with SUPERUSER and LOGIN could connect to the seed, so VACUUM (FREEZE, ANALYZE) was skipped; the seed is still cleanly shut down",
			attrs...)
	default:
		slog.Info("seed settle: the seed is recovered and cleanly shut down", attrs...)
	}
}

// Settle prepares a pg_basebackup seed for branching, in a helper on the
// branch image running as the in-image postgres user: it starts postgres on
// the seed once, which completes the base backup's recovery, runs VACUUM
// (FREEZE, ANALYZE) on every database (SettleFreeze), and stops it with a
// clean shutdown checkpoint. Branches then start without crash recovery, and
// a read in a branch no longer writes (no hint bits to set, nothing to prune,
// no anti-wraparound autovacuum), so on the overlay backend it copies
// nothing. See settleScript for how a production configuration is made safe
// to start.
//
// SettleOff does nothing. A seed from SeedDump needs no Settle: its helper
// already ends with a clean shutdown and applies SettleFreeze itself.
//
// The seed is an independent copy; the source is never touched. A failure to
// start, stop or verify the cluster is an error (tagged ErrSeedFailed: the
// usual cause is the source's configuration, and every branch would fail the
// same way); a failed VACUUM is logged and the seed kept.
func Settle(ctx context.Context, d runtime.Driver, s SeedSpec) error {
	mode, err := ParseSettleMode(string(s.Settle))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	if mode == SettleOff {
		slog.Info("seed settle: off; branches replay the base backup's WAL on first start and their reads may set hint bits", "addr", s.addr())
		return nil
	}
	slog.Info("seed settle: recovering the seed and shutting it down cleanly", "addr", s.addr(), "mode", mode, "image", s.Image)
	start := time.Now()
	out, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: s.Image,
		User:  s.helperUser(),
		Cmd:   []string{"sh", "-c", settleScript, "pgoverlay-settle", "/seed/data"},
		Env: []string{"PGB_SETTLE=" + string(mode), "PGB_USER=" + s.User,
			"PGCTLTIMEOUT=" + strconv.Itoa(settleWaitSeconds)},
		Mounts: []runtime.Mount{{Kind: s.MountKind, Volume: s.Volume, Target: "/seed"}},
	})
	if err != nil {
		return seedError{fmt.Errorf("settle the seed from %s (--seed-settle=%s; off skips this step): %w", s.addr(), mode, err)}
	}
	parseSettleReport(out).log("addr", s.addr(), "took", time.Since(start).Round(time.Millisecond))
	return nil
}
