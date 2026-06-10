#!/bin/bash
# hack/benchmark.sh — measured branching benchmarks (source of docs/benchmarks.md).
#
# For each target size it seeds a throwaway pgbench database in a local
# Postgres container, registers it as a pgbranch source, and measures:
#
#   seed time            wall time of `pgb source add` (pg_basebackup stream
#                        into the copy-on-write source volume)
#   branch create p50    median of N create/destroy cycles, parsed from the
#                        CLI's "ready in Xs" line
#   branch rw overhead   `du -sb` of the branch's rw volume right after
#                        creation (crash-recovery WAL + entrypoint; the same
#                        measurement the usage API performs)
#   write amplification  UPDATE 1% of pgbench_accounts rows on the branch,
#                        CHECKPOINT, re-measure the rw volume
#
# pgbench scale factors are calibrated empirically: a scale-10 database is
# initialized first and bytes-per-scale derived from pg_database_size.
#
# Sizes that do not fit in the Docker VM's free disk (each run needs roughly
# 2.2x the target size: the seeded database plus the source volume copy, plus
# WAL headroom) are skipped with a message.
#
# Requirements: docker, ./bin/pgb (make build). Everything this script
# creates — source container, pgbranch volumes, temp registry — is removed on
# exit, including on failure.
#
# Usage:
#   hack/benchmark.sh                      # default sizes: 1 and 5 GiB
#   BENCH_SIZES_GIB="1 5 10" hack/benchmark.sh
#
# Output: a markdown results table on stdout (progress on stderr); the raw
# per-size JSON lines are kept in a tmp file whose path is printed at the end.
set -euo pipefail

SIZES_GIB="${BENCH_SIZES_GIB:-1 5}"
RUNS="${BENCH_RUNS:-5}"
PG_IMAGE="${BENCH_PG_IMAGE:-postgres:17}"
HELPER_IMAGE="alpine:3.21"

SRC_CONTAINER="pgbranch-bench-source"
SOURCE="benchsrc"
BRANCH="benchwork"
PGPASS="pgbranch-bench"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PGB="$ROOT/bin/pgb"
[ -x "$PGB" ] || { echo "error: $PGB not found — run 'make build' first" >&2; exit 1; }

PGBRANCH_HOME="$(mktemp -d "${TMPDIR:-/tmp}/pgbranch-bench-home.XXXXXX")"
export PGBRANCH_HOME
RESULTS_JSON="$(mktemp "${TMPDIR:-/tmp}/pgbranch-bench-results.XXXXXX")"

log() { echo "[bench] $*" >&2; }

cleanup() {
    status=$?
    set +e
    trap - EXIT INT TERM
    log "cleaning up (exit status $status)"
    "$PGB" branch destroy "$BRANCH" >/dev/null 2>&1
    "$PGB" source rm "$SOURCE" >/dev/null 2>&1
    docker rm -f "pgbranch-br-$BRANCH" >/dev/null 2>&1
    docker rm -f "$SRC_CONTAINER" >/dev/null 2>&1
    docker volume rm -f "pgbranch-br-$BRANCH-rw" >/dev/null 2>&1
    docker volume ls -q | grep "^pgbranch-src-$SOURCE" | while read -r v; do
        docker volume rm -f "$v" >/dev/null 2>&1
    done
    rm -rf "$PGBRANCH_HOME"
    exit "$status"
}
trap cleanup EXIT INT TERM

psql_src() { docker exec "$SRC_CONTAINER" psql -U postgres -d postgres "$@"; }

# Free KiB on the filesystem Docker stores volumes on (a container's / is an
# overlay on that filesystem, so df inside a one-shot container measures it).
free_kib() { docker run --rm "$HELPER_IMAGE" df -Pk / | awk 'NR==2{print $4}'; }

# Branch rw volume size in bytes — same du -sb the engine's usage API runs.
rw_usage() {
    docker run --rm -v "pgbranch-br-$BRANCH-rw:/rw:ro" "$HELPER_IMAGE" \
        du -sb /rw | awk '{print $1}'
}

# Go duration string ("2.533s", "853ms", "1m2.5s") -> seconds with millis.
dur_to_secs() {
    echo "$1" | awk '{
        d = $0; t = 0
        if (d ~ /h/)      { split(d, a, "h"); t += a[1] * 3600; d = a[2] }
        if (d ~ /m[0-9]/) { split(d, a, "m"); t += a[1] * 60;   d = a[2] }
        if (d ~ /ms$/)         { sub(/ms$/, "", d); t += d / 1000 }
        else if (d ~ /[µu]s$/) { sub(/[µu]s$/, "", d) }  # sub-ms: call it 0
        else if (d ~ /s$/)     { sub(/s$/, "", d); t += d }
        printf "%.3f", t
    }'
}

# ---------------------------------------------------------------- source ----
if docker ps -a --format '{{.Names}}' | grep -qx "$SRC_CONTAINER"; then
    echo "error: container $SRC_CONTAINER already exists — remove it first" >&2
    exit 1
fi

log "starting seed container $SRC_CONTAINER ($PG_IMAGE)"
docker run -d --name "$SRC_CONTAINER" -e POSTGRES_PASSWORD="$PGPASS" \
    "$PG_IMAGE" -c wal_level=replica -c max_wal_senders=4 >/dev/null
for _ in $(seq 1 60); do
    docker exec "$SRC_CONTAINER" pg_isready -U postgres >/dev/null 2>&1 && break
    sleep 1
done
docker exec "$SRC_CONTAINER" pg_isready -U postgres >/dev/null

# The stock postgres image's pg_hba.conf has no remote replication entry.
docker exec "$SRC_CONTAINER" sh -c \
    'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
psql_src -c "SELECT pg_reload_conf();" >/dev/null
SRC_IP=$(docker inspect -f '{{.NetworkSettings.IPAddress}}' "$SRC_CONTAINER")

# ----------------------------------------------------------- calibration ----
log "calibrating pgbench bytes-per-scale (scale 10)"
BASE_BYTES=$(psql_src -tAc "SELECT pg_database_size(current_database())")
docker exec "$SRC_CONTAINER" pgbench -i -q -s 10 -U postgres postgres >/dev/null 2>&1
psql_src -c "CHECKPOINT" >/dev/null
CAL_BYTES=$(psql_src -tAc "SELECT pg_database_size(current_database())")
PER_SCALE=$(( (CAL_BYTES - BASE_BYTES) / 10 ))
log "empty db ${BASE_BYTES} B, scale-10 db ${CAL_BYTES} B -> ${PER_SCALE} B/scale"

scale_for_gib() {
    awk -v g="$1" -v p="$PER_SCALE" -v b="$BASE_BYTES" \
        'BEGIN { printf "%d", (g * 1073741824 - b) / p + 0.5 }'
}

# ------------------------------------------------------------- one size  ----
run_size() {
    gib=$1
    # ~2.2x target (database + source volume + WAL headroom) + 2 GiB margin
    need_kib=$(awk -v g="$gib" 'BEGIN { printf "%d", g * 1048576 * 2.2 + 2097152 }')
    have_kib=$(free_kib)
    if [ "$have_kib" -lt "$need_kib" ]; then
        log "SKIP ${gib} GiB: needs ~$(( need_kib / 1024 / 1024 )) GiB free in the Docker VM, have $(( have_kib / 1024 / 1024 )) GiB"
        return 0
    fi

    scale=$(scale_for_gib "$gib")
    log "--- ${gib} GiB target: pgbench -i -s $scale ---"
    docker exec "$SRC_CONTAINER" pgbench -i -q -s "$scale" -U postgres postgres >/dev/null 2>&1
    psql_src -c "CHECKPOINT" >/dev/null
    db_bytes=$(psql_src -tAc "SELECT pg_database_size(current_database())")
    log "database size: $db_bytes B"

    log "seeding source (pg_basebackup)"
    t0=$(date +%s)
    PGPASSWORD="$PGPASS" "$PGB" source add "$SOURCE" --host "$SRC_IP" --user postgres >/dev/null
    seed_s=$(( $(date +%s) - t0 ))
    log "seeded in ${seed_s}s"

    times=""
    i=1
    while [ "$i" -le "$RUNS" ]; do
        out=$("$PGB" branch create "$BRANCH" --from "$SOURCE")
        dur=$(echo "$out" | sed -E 's/.*ready in ([^ ]+) .*/\1/')
        secs=$(dur_to_secs "$dur")
        log "create run $i/$RUNS: ${secs}s"
        times="$times $secs"
        if [ "$i" -lt "$RUNS" ]; then
            "$PGB" branch destroy "$BRANCH" >/dev/null
        fi
        i=$(( i + 1 ))
    done
    # shellcheck disable=SC2086  # word-splitting times is intended
    p50=$(printf '%s\n' $times | sort -n | awk -v n="$RUNS" 'NR == int((n + 1) / 2)')
    runs_csv=$(echo "$times" | awk '{ out = $1; for (i = 2; i <= NF; i++) out = out "," $i; print out }')

    overhead=$(rw_usage)
    log "rw overhead after create: $overhead B"

    rows=$(( scale * 1000 ))  # pgbench_accounts has scale*100000 rows; 1% = scale*1000
    log "write-amplification probe: UPDATE $rows rows (1%) on the branch"
    docker exec "pgbranch-br-$BRANCH" psql -U postgres -d postgres -q \
        -c "UPDATE pgbench_accounts SET abalance = abalance + 1 WHERE aid <= $rows" \
        -c "CHECKPOINT"
    post=$(rw_usage)
    log "rw after 1% update: $post B"

    "$PGB" branch destroy "$BRANCH" >/dev/null
    "$PGB" source rm "$SOURCE" >/dev/null
    # Drop the pgbench data so the next size's disk check sees real free space.
    psql_src -q -c "DROP TABLE IF EXISTS pgbench_accounts, pgbench_branches, pgbench_history, pgbench_tellers" \
        -c "CHECKPOINT" >/dev/null

    printf '{"size_gib":%s,"scale":%s,"db_bytes":%s,"seed_seconds":%s,"create_runs_s":[%s],"create_p50_s":%s,"rw_overhead_bytes":%s,"updated_rows":%s,"rw_after_update_bytes":%s}\n' \
        "$gib" "$scale" "$db_bytes" "$seed_s" "$runs_csv" "$p50" "$overhead" "$rows" "$post" \
        >>"$RESULTS_JSON"
}

for gib in $SIZES_GIB; do
    run_size "$gib"
done

# -------------------------------------------------------- markdown emit  ----
log "raw results: $RESULTS_JSON"
awk '
function gib(b) { return sprintf("%.2f GiB", b / 1073741824) }
function mib(b) { return sprintf("%.1f MiB", b / 1048576) }
function field(line, key,    re, v) {
    re = "\"" key "\":\\[?[0-9.]+"
    if (!match(line, re)) return ""
    v = substr(line, RSTART, RLENGTH)
    sub(/^[^:]*:\[?/, "", v)
    return v
}
BEGIN {
    print "| Database size | pgbench scale | Seed time | Branch create (p50 of 5) | Branch rw overhead | rw after 1% update |"
    print "|---|---|---|---|---|---|"
}
{
    printf "| %s | %s | %s s | %s s | %s | %s |\n",
        gib(field($0, "db_bytes")), field($0, "scale"),
        field($0, "seed_seconds"), field($0, "create_p50_s"),
        mib(field($0, "rw_overhead_bytes")), mib(field($0, "rw_after_update_bytes"))
}
' "$RESULTS_JSON"
