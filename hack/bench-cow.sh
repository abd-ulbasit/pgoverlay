#!/usr/bin/env bash
# hack/bench-cow.sh — copy-on-write (#49) pgbench benchmark for the overlay
# backend. It compares four configurations against one identical dataset, in
# interleaved A/B/C/D rounds so that load spikes on a shared host fall across
# all four rather than penalising whichever ran during them:
#
#   A  plain postgres on a normal Docker volume, no branch      the baseline
#   B  a branch with --lazyrw=off (eager: copy a file on open)  today's cost
#   C  a branch with the shim (default; Docker volumes, ext4)   the release
#   D  a branch with the shim and --volume-root on an XFS        reflink clone
#      (reflink=1) directory
#
# For B/C/D it records: branch create time; the branch rw-layer growth after a
# read-only workload (pgbench -S) and after a write workload (TPC-B); pgbench
# select-only and TPC-B throughput cold (the first run after create, which pays
# the copy-up first touch) and warm (after one warm-up run); and the first-write
# stall — the wall time of the first one-row UPDATE of a ~1 GiB single-segment
# table, and of the CHECKPOINT after it. With the shim the UPDATE only dirties a
# buffer; the segment is copied (C) or cloned (D) when the page is written out,
# which the timed CHECKPOINT captures. B has already paid it on the first read.
# A has no rw layer, so its rw columns are blank; it is the TPS ceiling.
#
# Interleaving, medians and the load average during every run are recorded
# because a benchmark host may be shared. The order of the configurations
# rotates from round to round (BENCH_ROTATE=1), so none of them always runs
# right after another one's copy-up. The gate compares C with B round by round
# (paired ratios) as well as by medians. The raw per-run JSON is kept (at
# $BENCH_JSON when set); the markdown table is written to the path in
# $BENCH_OUT (default stdout).
#
# This is NOT hack/benchmark.sh (branch create/seed scaling across DB sizes).
# This one holds the dataset fixed and measures the CoW machinery under pgbench.
#
# Requirements: a LOCAL Docker engine on a reflink-capable host (XFS reflink=1
# or btrfs) for configuration D, ./bin/pgb (make build), and an XFS/btrfs
# directory passed as $BENCH_VOLUME_ROOT for D. Without it, D is skipped.
# Everything the script creates is removed on exit, including on failure.
#
# Usage:
#   BENCH_VOLUME_ROOT=/data/pgoverlay/cow-bench/vr \
#   BENCH_OUT=results.md hack/bench-cow.sh
#
# Knobs (env): BENCH_ROUNDS=3  BENCH_DURATION=60  BENCH_CLIENTS=8
#              BENCH_JOBS=4  BENCH_SCALE=50  BENCH_STALL_ROWS=850000
#              BENCH_PG_IMAGE=postgres:17  BENCH_CONFIGS="A B C D"
#              BENCH_ROTATE=1  BENCH_JSON=<path for the raw per-run JSON>
set -euo pipefail

ROUNDS="${BENCH_ROUNDS:-3}"
DURATION="${BENCH_DURATION:-60}"
CLIENTS="${BENCH_CLIENTS:-8}"
JOBS="${BENCH_JOBS:-4}"
SCALE="${BENCH_SCALE:-50}"
STALL_ROWS="${BENCH_STALL_ROWS:-850000}" # ~950 MiB: one <1 GiB heap segment
PG_IMAGE="${BENCH_PG_IMAGE:-postgres:17}"
HELPER_IMAGE="alpine:3.21"
VOLUME_ROOT="${BENCH_VOLUME_ROOT:-}"
CONFIGS="${BENCH_CONFIGS:-A B C D}"
ROTATE="${BENCH_ROTATE:-1}"
OUT="${BENCH_OUT:-/dev/stdout}"

PGPASS="pgoverlay-cowbench"
SRC_CONTAINER="pgoverlay-cbench-src"
A_CONTAINER="pgoverlay-cbench-a"
SRC_EXT="cbext"  # source seeded onto Docker's own volumes (ext4)
SRC_XFS="cbxfs"  # source seeded onto the XFS volume-root (D)

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PGB="$ROOT/bin/pgb"
[ -x "$PGB" ] || { echo "error: $PGB not found — run 'make build' first" >&2; exit 1; }
command -v docker >/dev/null || { echo "error: docker not found" >&2; exit 1; }

PGOVERLAY_HOME="$(mktemp -d "${TMPDIR:-/tmp}/pgoverlay-cowbench-home.XXXXXX")"
export PGOVERLAY_HOME
if [ -n "${BENCH_JSON:-}" ]; then
    RESULTS_JSON="$BENCH_JSON"; : >"$RESULTS_JSON"; : >"$RESULTS_JSON.meta"
else
    RESULTS_JSON="$(mktemp "${TMPDIR:-/tmp}/pgoverlay-cowbench.XXXXXX.jsonl")"
fi

log() { echo "[bench-cow] $*" >&2; }

# ---- D availability -------------------------------------------------------
DO_D=0
case " $CONFIGS " in *" D "*)
    if [ -n "$VOLUME_ROOT" ]; then
        mkdir -p "$VOLUME_ROOT"
        fstype=$(docker run --rm -v "$VOLUME_ROOT:/vr" "$HELPER_IMAGE" \
            stat -f -c %T /vr 2>/dev/null || echo unknown)
        case "$fstype" in
            xfs|btrfs) DO_D=1 ;;
            *) log "WARN: \$BENCH_VOLUME_ROOT ($VOLUME_ROOT) is $fstype, not a reflink filesystem — skipping D" ;;
        esac
    else
        log "WARN: \$BENCH_VOLUME_ROOT is unset — skipping D (reflink clone config)"
    fi
    ;;
esac

want() { case " $CONFIGS " in *" $1 "*) [ "$1" != D ] || [ "$DO_D" = 1 ] ;; *) return 1 ;; esac; }

# ---- cleanup --------------------------------------------------------------
BRANCHES_UP=""
cleanup() {
    status=$?
    set +e
    trap - EXIT INT TERM
    log "cleaning up (exit status $status)"
    for b in $BRANCHES_UP; do "$PGB" branch destroy "$b" >/dev/null 2>&1; done
    docker rm -f -v "$A_CONTAINER" >/dev/null 2>&1
    "$PGB" source rm "$SRC_EXT" >/dev/null 2>&1
    PGOVERLAY_VOLUME_ROOT="$VOLUME_ROOT" "$PGB" source rm "$SRC_XFS" >/dev/null 2>&1
    docker rm -f -v "$SRC_CONTAINER" >/dev/null 2>&1
    # stragglers
    docker ps -aq --filter "name=pgoverlay-br-cbench-" | xargs -r docker rm -f -v >/dev/null 2>&1
    docker volume ls -q | grep -E "^(pgoverlay-br-cbench-|pgoverlay-src-cbext|pgoverlay-src-cbxfs|pgoverlay-cbench-a-)" \
        | xargs -r docker volume rm -f >/dev/null 2>&1
    [ -n "$VOLUME_ROOT" ] && rm -rf "${VOLUME_ROOT:?}"/pgoverlay-* 2>/dev/null
    rm -rf "$PGOVERLAY_HOME"
    log "raw per-run JSON kept at $RESULTS_JSON"
    exit "$status"
}
trap cleanup EXIT INT TERM

# ---- small helpers --------------------------------------------------------
loadavg() { cut -d' ' -f1 /proc/loadavg 2>/dev/null || echo 0; }
now_ns() { date +%s.%N; }
elapsed() { awk -v a="$1" -v b="$2" 'BEGIN{printf "%.3f", b-a}'; }

# Go duration ("2.53s", "853ms", "1m2.5s") -> seconds.
dur_to_secs() {
    echo "$1" | awk '{ d=$0; t=0
        if (d ~ /h/)      { split(d,a,"h"); t+=a[1]*3600; d=a[2] }
        if (d ~ /m[0-9]/) { split(d,a,"m"); t+=a[1]*60;   d=a[2] }
        if (d ~ /ms$/)         { sub(/ms$/,"",d); t+=d/1000 }
        else if (d ~ /[µu]s$/) { sub(/[µu]s$/,"",d) }
        else if (d ~ /s$/)     { sub(/s$/,"",d); t+=d }
        printf "%.3f", t }'
}

psql_src() { docker exec "$SRC_CONTAINER" psql -U postgres -d postgres "$@"; }

# The branch's rw volume, from its container's mounts (its name is not
# derivable: recreate-after-destroy takes a new generation).
rw_volume() { docker inspect -f '{{range .Mounts}}{{if eq .Destination "/pgoverlay/rw"}}{{.Name}}{{end}}{{end}}' "$1"; }

# Bytes in a branch's rw volume. du -sb (apparent, what the usage API reports
# in copy mode) and, in clone mode, pgoverlay-du exclusive bytes (extents the
# branch owns alone). Prints "<apparent> <exclusive>"; exclusive is "-" when
# not measured.
DU_TOOL=""  # host path to a pgoverlay-du binary for clone-mode accounting
rw_usage() {
    local cont="$1" vol du_out ex="-"
    vol=$(rw_volume "$cont")
    [ -n "$vol" ] || { echo "0 -"; return; }
    du_out=$(docker run --rm -v "$vol:/rw:ro" "$HELPER_IMAGE" du -sb /rw | awk '{print $1}')
    if [ -n "$DU_TOOL" ]; then
        ex=$(docker run --rm -v "$vol:/rw:ro" -v "$DU_TOOL:/pgoverlay-du:ro" "$HELPER_IMAGE" \
            /pgoverlay-du -- /rw 2>/dev/null | awk '{print $1}')
        [ -n "$ex" ] || ex="-"
    fi
    echo "$du_out $ex"
}

# pgbench inside a container against the local socket; prints TPS.
pgbench_tps() {
    local cont="$1"; shift
    docker exec "$cont" pgbench -n -U postgres "$@" -c "$CLIENTS" -j "$JOBS" -T "$DURATION" postgres 2>/dev/null \
        | awk '/^tps = /{print $3; exit}'
}

# Wall time (seconds) of one SQL statement: timed_sql <container> <sql>.
timed_sql() {
    local cont="$1" t0 t1
    t0=$(now_ns)
    docker exec "$cont" psql -U postgres -d postgres -q -c "$2" >/dev/null 2>&1
    t1=$(now_ns)
    elapsed "$t0" "$t1"
}

wait_ready() {
    local cont="$1" _
    for _ in $(seq 1 120); do
        docker exec "$cont" pg_isready -U postgres -d postgres >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

emit() { # emit <config> <round> <k=v>...
    local cfg="$1" round="$2"; shift 2
    local json="{\"config\":\"$cfg\",\"round\":$round"
    for kv in "$@"; do json="$json,\"${kv%%=*}\":${kv#*=}"; done
    echo "$json}" >>"$RESULTS_JSON"
}

# ============================ SOURCE SETUP =================================
if docker ps -a --format '{{.Names}}' | grep -qx "$SRC_CONTAINER"; then
    echo "error: $SRC_CONTAINER already exists — remove it first" >&2; exit 1
fi
log "starting seed source $SRC_CONTAINER ($PG_IMAGE)"
docker run -d --name "$SRC_CONTAINER" -e POSTGRES_PASSWORD="$PGPASS" \
    "$PG_IMAGE" -c wal_level=replica -c max_wal_senders=4 >/dev/null
wait_ready "$SRC_CONTAINER"
docker exec "$SRC_CONTAINER" sh -c \
    'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
psql_src -c "SELECT pg_reload_conf();" >/dev/null

log "pgbench -i -s $SCALE (~$((SCALE*15)) MiB) + a ~1 GiB single-segment stall table"
docker exec "$SRC_CONTAINER" pgbench -i -q -s "$SCALE" -U postgres postgres >/dev/null 2>&1
# ~1.1 KiB rows -> ~7 rows/8 KiB page; STALL_ROWS chosen to stay under 1 GiB.
psql_src -q \
    -c "CREATE TABLE stall_big (id int primary key, pad text)" \
    -c "INSERT INTO stall_big SELECT g, repeat('x',1000) FROM generate_series(1,$STALL_ROWS) g" \
    -c "VACUUM (FREEZE, ANALYZE) stall_big" \
    -c "CHECKPOINT" >/dev/null
seg=$(psql_src -tAc "SELECT pg_relation_size('stall_big')")
log "stall_big is $seg B ($(awk -v b="$seg" 'BEGIN{printf "%.0f", b/1048576}') MiB)$([ "$seg" -ge 1073741824 ] && echo ' — WARN: >=1 GiB, spilled to a 2nd segment; lower BENCH_STALL_ROWS')"
psql_src -c "VACUUM (FREEZE, ANALYZE)" >/dev/null 2>&1 || true
SRC_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$SRC_CONTAINER")
[ -n "$SRC_IP" ] || { log "no source IP"; exit 1; }

log "seeding source '$SRC_EXT' onto Docker volumes (ext4) — for A, B, C"
PGPASSWORD="$PGPASS" "$PGB" source add "$SRC_EXT" --host "$SRC_IP" --user postgres --pg-version "${PG_IMAGE##*:}" >/dev/null 2>&1 \
    || PGPASSWORD="$PGPASS" "$PGB" source add "$SRC_EXT" --host "$SRC_IP" --user postgres >/dev/null

if want D; then
    log "setting XFS cowextsize hint (16 KiB) on $VOLUME_ROOT and locating pgoverlay-du"
    # locate the committed pgoverlay-du for this host arch and extract it
    arch=$(uname -m)
    du_src="$ROOT/internal/cow/usage/dist/pgoverlay-du-$arch"
    if [ -f "$du_src" ]; then
        DU_TOOL="$PGOVERLAY_HOME/pgoverlay-du"; cp "$du_src" "$DU_TOOL"; chmod 0755 "$DU_TOOL"
        docker run --rm -v "$VOLUME_ROOT:/vr" -v "$DU_TOOL:/pgoverlay-du:ro" "$HELPER_IMAGE" \
            /pgoverlay-du -c 16384 /vr >/dev/null 2>&1 || log "WARN: could not set cowextsize hint"
    else
        log "WARN: no pgoverlay-du-$arch in dist — D usage falls back to du -sb"
    fi
    log "seeding source '$SRC_XFS' onto $VOLUME_ROOT (XFS reflink) — for D"
    PGOVERLAY_VOLUME_ROOT="$VOLUME_ROOT" PGPASSWORD="$PGPASS" \
        "$PGB" source add "$SRC_XFS" --host "$SRC_IP" --user postgres --pg-version "${PG_IMAGE##*:}" >/dev/null 2>&1 \
        || PGOVERLAY_VOLUME_ROOT="$VOLUME_ROOT" PGPASSWORD="$PGPASS" \
           "$PGB" source add "$SRC_XFS" --host "$SRC_IP" --user postgres >/dev/null
fi

# copy-up probe evidence, per filesystem, from the real branchd: it logs
# "copy-up probe: <result>" from a startup goroutine. We start it, capture that
# line, and stop it. Best-effort; the benchmark does not depend on it.
BRANCHD="$ROOT/bin/branchd"
probe_fs() { # probe_fs <label> <volume-root-or-empty>
    local vr="$2" logf line
    [ -x "$BRANCHD" ] || { log "copy-up probe ($1): branchd not built"; return 0; }
    logf="$PGOVERLAY_HOME/branchd-$1.log"
    local tok; tok=$(openssl rand -hex 16 2>/dev/null || echo 0123456789abcdef0123456789abcdef)
    PGOVERLAY_VOLUME_ROOT="$vr" PGOVERLAY_TOKEN="$tok" \
        "$BRANCHD" --api-addr 127.0.0.1:0 --pg-addr 127.0.0.1:0 >"$logf" 2>&1 &
    local pid=$!
    for _ in $(seq 1 30); do
        line=$(grep -o 'copy-up probe:.*' "$logf" 2>/dev/null | head -1) && [ -n "$line" ] && break
        kill -0 "$pid" 2>/dev/null || break
        sleep 1
    done
    kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
    line=$(grep -o 'copy-up probe:.*' "$logf" 2>/dev/null | head -1)
    log "copy-up probe ($1): ${line:-not captured (see $logf)}"
    echo "probe $1: ${line:-not captured}" >>"$RESULTS_JSON.meta"
}

log "source(s) ready; PGOVERLAY_HOME=$PGOVERLAY_HOME"
echo "# host: $(uname -srm); nproc=$(nproc 2>/dev/null); mem=$(free -m 2>/dev/null | awk '/^Mem:/{print $2" MiB"}'); loadavg at start=$(cat /proc/loadavg 2>/dev/null)" >>"$RESULTS_JSON.meta"
echo "# stall_big: $seg B; pgbench scale $SCALE; images $PG_IMAGE" >>"$RESULTS_JSON.meta"
probe_fs ext4 "" || true
want D && { probe_fs xfs "$VOLUME_ROOT" || true; }

# ============================ MEASURE ONE CONFIG ===========================
# For A: fresh plain postgres on a fresh ext4 volume, copied from the ext4
# source's data dir (identical bytes). For B/C/D: a fresh branch. Then the
# same run sequence, so every config is measured identically.
measure() { # measure <config> <round> <position in the round>
    local cfg="$1" round="$2" br="cbench-${1,,}-r${2}" cont
    local t0 t1 create_s la_start la_end
    la_start=$(loadavg)

    if [ "$cfg" = A ]; then
        cont="$A_CONTAINER"
        docker rm -f -v "$cont" >/dev/null 2>&1
        local vol="pgoverlay-cbench-a-data-r${round}"
        docker volume rm -f "$vol" >/dev/null 2>&1; docker volume create "$vol" >/dev/null
        # copy the ext4 source's settled data dir into A's own volume
        docker run --rm -v "pgoverlay-src-$SRC_EXT:/src:ro" -v "$vol:/dst" "$HELPER_IMAGE" \
            sh -c 'cp -a /src/data/. /dst/ && rm -f /dst/postmaster.pid' >/dev/null
        t0=$(now_ns)
        docker run -d --name "$cont" -v "$vol:/var/lib/postgresql/data" \
            -e POSTGRES_PASSWORD="$PGPASS" "$PG_IMAGE" >/dev/null
        wait_ready "$cont" || { log "A did not become ready"; return 1; }
        t1=$(now_ns); create_s=$(elapsed "$t0" "$t1")
    else
        local env=() ; cont="pgoverlay-br-$br"
        [ "$cfg" = B ] && env+=(PGOVERLAY_LAZYRW=off)
        [ "$cfg" = D ] && env+=(PGOVERLAY_VOLUME_ROOT="$VOLUME_ROOT")
        local from="$SRC_EXT"; [ "$cfg" = D ] && from="$SRC_XFS"
        t0=$(now_ns)
        local out; out=$(env "${env[@]}" "$PGB" branch create "$br" --from "$from")
        t1=$(now_ns)
        BRANCHES_UP="$BRANCHES_UP $br"
        # prefer the CLI's own "ready in Xs"; fall back to wall time
        local dur; dur=$(printf '%s' "$out" | sed -nE 's/.*ready in ([^ ]+) .*/\1/p' | head -1)
        if [ -n "$dur" ]; then create_s=$(dur_to_secs "$dur"); else create_s=$(elapsed "$t0" "$t1"); fi
        wait_ready "$cont" || { log "$cfg branch $br not ready"; return 1; }
    fi

    log "=== $cfg round $round ($cont) create=${create_s}s loadavg=$la_start ==="

    # rw growth is meaningful only for branches
    local rw0="-" rw_ro="-" rw_ro_ex="-" rw_tpcb="-" rw_tpcb_ex="-"
    if [ "$cfg" != A ]; then read -r rw0 _ < <(rw_usage "$cont"); fi

    # COLD select-only (first run after create)
    local sS_cold sS_warm tpcb_cold tpcb_warm
    sS_cold=$(pgbench_tps "$cont" -S)
    if [ "$cfg" != A ]; then read -r rw_ro rw_ro_ex < <(rw_usage "$cont"); fi
    # COLD TPC-B (first write workload -> first-touch copy-up on C)
    tpcb_cold=$(pgbench_tps "$cont")
    if [ "$cfg" != A ]; then read -r rw_tpcb rw_tpcb_ex < <(rw_usage "$cont"); fi
    # WARM (after one warm-up run each)
    sS_warm=$(pgbench_tps "$cont" -S)
    tpcb_warm=$(pgbench_tps "$cont")

    # first-write stall on the untouched ~1 GiB table (A writes to its plain
    # volume, recorded for reference). An untimed CHECKPOINT first flushes the
    # TPC-B runs' dirty pages. Then the one-row UPDATE (B already holds the
    # segment; C and D only dirty a buffer) and the CHECKPOINT that writes the
    # page out, which is where C copies the segment and D clones it.
    local stall stall_ckpt
    timed_sql "$cont" "CHECKPOINT" >/dev/null
    stall=$(timed_sql "$cont" "UPDATE stall_big SET pad = pad WHERE id = 1")
    stall_ckpt=$(timed_sql "$cont" "CHECKPOINT")
    la_end=$(loadavg)

    emit "$cfg" "$round" \
        "create_s=$create_s" \
        "rw_after_create=${rw0:--}" \
        "rw_after_ro=${rw_ro:--}" "rw_after_ro_excl=${rw_ro_ex:--}" \
        "rw_after_tpcb=${rw_tpcb:--}" "rw_after_tpcb_excl=${rw_tpcb_ex:--}" \
        "sel_cold_tps=${sS_cold:-0}" "sel_warm_tps=${sS_warm:-0}" \
        "tpcb_cold_tps=${tpcb_cold:-0}" "tpcb_warm_tps=${tpcb_warm:-0}" \
        "stall_s=$stall" "stall_ckpt_s=$stall_ckpt" "pos=$3" \
        "loadavg_start=$la_start" "loadavg_end=$la_end"

    # tear down so peak disk stays at one config at a time
    if [ "$cfg" = A ]; then
        docker rm -f -v "$cont" >/dev/null 2>&1
        docker volume rm -f "pgoverlay-cbench-a-data-r${round}" >/dev/null 2>&1
    else
        "$PGB" branch destroy "$br" >/dev/null 2>&1
        BRANCHES_UP="${BRANCHES_UP/ $br/}"
    fi
}

# ============================ INTERLEAVED ROUNDS ===========================
# Round r runs the wanted configurations rotated left by r-1 (A B C D, then
# B C D A, ...), so each one takes every position in a round equally often.
order=()
for cfg in $CONFIGS; do want "$cfg" && order+=("$cfg"); done
for r in $(seq 1 "$ROUNDS"); do
    off=0; [ "$ROTATE" = 1 ] && off=$(( (r - 1) % ${#order[@]} ))
    log "########## ROUND $r/$ROUNDS ##########"
    for i in "${!order[@]}"; do
        cfg="${order[$(( (i + off) % ${#order[@]} ))]}"
        measure "$cfg" "$r" "$((i + 1))" || log "WARN: $cfg round $r failed; continuing"
    done
done

# ============================ AGGREGATE + EMIT =============================
log "aggregating; raw JSON: $RESULTS_JSON"
{
    echo "<!-- generated by hack/bench-cow.sh -->"
    [ -f "$RESULTS_JSON.meta" ] && sed 's/^# /Host: /' "$RESULTS_JSON.meta"
    echo
    awk -v rounds="$ROUNDS" -v dur="$DURATION" -v cl="$CLIENTS" -v jb="$JOBS" -v sc="$SCALE" -v rot="$ROTATE" '
    function num(line,key,   re,v){ re="\""key"\":[-0-9.]+"; if(!match(line,re))return "";
        v=substr(line,RSTART,RLENGTH); sub(/^[^:]*:/,"",v); return v }
    function str(line,key,   re,v){ re="\""key"\":\"[^\"]*\""; if(!match(line,re))return "";
        v=substr(line,RSTART,RLENGTH); sub(/^[^:]*:"/,"",v); sub(/"$/,"",v); return v }
    function push(cfg,key,val,   i){ if(val==""||val=="-")return; i=++n[cfg,key]; V[cfg,key,i]=val+0 }
    function median(cfg,key,   i,c,a,m){ c=n[cfg,key]; if(c==0)return "";
        for(i=1;i<=c;i++)a[i]=V[cfg,key,i];
        for(i=1;i<=c;i++)for(m=i+1;m<=c;m++)if(a[m]<a[i]){t=a[i];a[i]=a[m];a[m]=t}
        if(c%2)return a[(c+1)/2]; return (a[c/2]+a[c/2+1])/2 }
    function lo(cfg,key,   i,c,m){ c=n[cfg,key]; if(c==0)return ""; m=V[cfg,key,1];
        for(i=2;i<=c;i++)if(V[cfg,key,i]<m)m=V[cfg,key,i]; return m }
    function hi(cfg,key,   i,c,m){ c=n[cfg,key]; if(c==0)return ""; m=V[cfg,key,1];
        for(i=2;i<=c;i++)if(V[cfg,key,i]>m)m=V[cfg,key,i]; return m }
    function spread(cfg,key,   l,h){ l=lo(cfg,key); if(l=="")return "—"; h=hi(cfg,key);
        return sprintf("%.0f (%.0f–%.0f)", median(cfg,key), l, h) }
    function spread2(cfg,key,   l,h){ l=lo(cfg,key); if(l=="")return "—"; h=hi(cfg,key);
        return sprintf("%.2f (%.2f–%.2f)", median(cfg,key), l, h) }
    function mib(cfg,key,   m){ m=median(cfg,key); if(m=="")return "—"; return sprintf("%.1f", m/1048576) }
    /^{/{ line=$0; c=str(line,"config"); cfgs[c]=1;
        push(c,"create_s",num(line,"create_s"))
        push(c,"rw_after_create",num(line,"rw_after_create"))
        push(c,"rw_after_ro",num(line,"rw_after_ro"))
        push(c,"rw_after_ro_excl",num(line,"rw_after_ro_excl"))
        push(c,"rw_after_tpcb",num(line,"rw_after_tpcb"))
        push(c,"rw_after_tpcb_excl",num(line,"rw_after_tpcb_excl"))
        push(c,"sel_cold_tps",num(line,"sel_cold_tps"))
        push(c,"sel_warm_tps",num(line,"sel_warm_tps"))
        push(c,"tpcb_cold_tps",num(line,"tpcb_cold_tps"))
        push(c,"tpcb_warm_tps",num(line,"tpcb_warm_tps"))
        push(c,"stall_s",num(line,"stall_s"))
        push(c,"stall_ckpt_s",num(line,"stall_ckpt_s"))
        r=num(line,"round"); rounds_seen[r]=1
        v=num(line,"sel_warm_tps"); if(v!=""&&v>0)SW[c,r]=v+0
        v=num(line,"tpcb_warm_tps"); if(v!=""&&v>0)TW[c,r]=v+0
        push(c,"loadavg_start",num(line,"loadavg_start"))
        push(c,"loadavg_end",num(line,"loadavg_end"))
    }
    END{
        split("A B C D",order," ")
        printf "Interleaved A/B/C/D%s, %d rounds, pgbench -c%d -j%d -T%d, scale %d. Median (min–max).\n\n", (rot=="1"?" (order rotated each round)":""),rounds,cl,jb,dur,sc
        print  "| Metric | A plain (baseline) | B eager (--lazyrw=off) | C shim (ext4) | D shim + XFS clone |"
        print  "|---|---|---|---|---|"
        printf "| Branch create (s) | %s | %s | %s | %s |\n", spread2("A","create_s"),spread2("B","create_s"),spread2("C","create_s"),spread2("D","create_s")
        printf "| rw after create (MiB) | — | %s | %s | %s |\n", mib("B","rw_after_create"),mib("C","rw_after_create"),mib("D","rw_after_create")
        printf "| rw after read-only (MiB) | — | %s | %s | %s |\n", mib("B","rw_after_ro"),mib("C","rw_after_ro"),mib("D","rw_after_ro")
        printf "| rw after read-only, exclusive (MiB) | — | %s | %s | %s |\n", mib("B","rw_after_ro_excl"),mib("C","rw_after_ro_excl"),mib("D","rw_after_ro_excl")
        printf "| rw after TPC-B (MiB) | — | %s | %s | %s |\n", mib("B","rw_after_tpcb"),mib("C","rw_after_tpcb"),mib("D","rw_after_tpcb")
        printf "| rw after TPC-B, exclusive (MiB) | — | %s | %s | %s |\n", mib("B","rw_after_tpcb_excl"),mib("C","rw_after_tpcb_excl"),mib("D","rw_after_tpcb_excl")
        printf "| select-only TPS, cold | %s | %s | %s | %s |\n", spread("A","sel_cold_tps"),spread("B","sel_cold_tps"),spread("C","sel_cold_tps"),spread("D","sel_cold_tps")
        printf "| select-only TPS, warm | %s | %s | %s | %s |\n", spread("A","sel_warm_tps"),spread("B","sel_warm_tps"),spread("C","sel_warm_tps"),spread("D","sel_warm_tps")
        printf "| TPC-B TPS, cold | %s | %s | %s | %s |\n", spread("A","tpcb_cold_tps"),spread("B","tpcb_cold_tps"),spread("C","tpcb_cold_tps"),spread("D","tpcb_cold_tps")
        printf "| TPC-B TPS, warm | %s | %s | %s | %s |\n", spread("A","tpcb_warm_tps"),spread("B","tpcb_warm_tps"),spread("C","tpcb_warm_tps"),spread("D","tpcb_warm_tps")
        printf "| first-write stall: UPDATE (s) | %s | %s | %s | %s |\n", spread2("A","stall_s"),spread2("B","stall_s"),spread2("C","stall_s"),spread2("D","stall_s")
        printf "| first-write stall: CHECKPOINT after it (s) | %s | %s | %s | %s |\n", spread2("A","stall_ckpt_s"),spread2("B","stall_ckpt_s"),spread2("C","stall_ckpt_s"),spread2("D","stall_ckpt_s")
        printf "| loadavg during runs | %s | %s | %s | %s |\n", spread2("A","loadavg_start"),spread2("B","loadavg_start"),spread2("C","loadavg_start"),spread2("D","loadavg_start")
        print ""
        # Paired, per-round ratios: within one round the configurations ran
        # minutes apart, so a ratio cancels most drift in the host.
        print "Per-round ratios of warm throughput (same round, so host drift cancels): median (min–max)."
        print ""
        print "| Ratio | select-only, warm | TPC-B, warm |"
        print "|---|---|---|"
        split("C/B C/A B/A D/C", pairs, " ")
        for(p=1;p<=4;p++){ split(pairs[p],xy,"/")
            s1=pair(SW,xy[1],xy[2]); s2=pair(TW,xy[1],xy[2])
            if(s1!=""||s2!="") printf "| %s | %s | %s |\n", pairs[p], (s1==""?"—":s1), (s2==""?"—":s2) }
        print ""
        # Release gate. The shim is judged against the eager branch B, which
        # has the same overlay and differs only by the shim; C against plain A
        # is reported beside it. Noise: the run-to-run spread (max-min)/median
        # of the baselines A and B themselves, with a floor of 2%.
        sw_a=median("A","sel_warm_tps"); sw_b=median("B","sel_warm_tps"); sw_c=median("C","sel_warm_tps")
        tw_b=median("B","tpcb_warm_tps"); tw_c=median("C","tpcb_warm_tps")
        print "## Release gate"
        if(sw_c!=""&&sw_b!=""){ d=(sw_c-sw_b)/sw_b*100; pr=pmed(SW,"C","B")
            noise=relspread("B","sel_warm_tps"); if(sw_a!=""&&relspread("A","sel_warm_tps")>noise)noise=relspread("A","sel_warm_tps")
            if(noise<2)noise=2
            printf "- select-only, warm: C %.0f vs B %.0f (%+.1f%% on medians; paired C/B median %.3f)", sw_c,sw_b,d,pr
            if(sw_a!="") printf "; vs A %.0f (%+.1f%%; paired C/A %.3f, B/A %.3f)", sw_a,(sw_c-sw_a)/sw_a*100,pmed(SW,"C","A"),pmed(SW,"B","A")
            printf "; noise band ±%.1f%%: %s\n", noise, ((d>=-noise&&d<=noise&&(pr-1)*100>=-noise&&(pr-1)*100<=noise)?"PASS (within noise of B)":"FAIL (outside the noise band)") }
        if(tw_c!=""&&tw_b!=""){ d=(tw_c-tw_b)/tw_b*100; pr=pmed(TW,"C","B"); nb=pbelow(TW,"C","B",0.95); np=pcount(TW,"C","B")
            printf "- TPC-B, warm: C %.0f vs B %.0f (%+.1f%% on medians); paired C/B median %.3f, %d of %d rounds below 0.95; B spread %.1f%%, C spread %.1f%%\n", tw_c,tw_b,d,pr,nb,np,relspread("B","tpcb_warm_tps"),relspread("C","tpcb_warm_tps")
            if(d>=-5.0&&pr>=0.95) v="PASS (C within 5% of B on warm TPC-B, by medians and by paired rounds)"
            # FAIL needs the medians, the paired median and at least 80% of the
            # paired rounds under the bar (a sign test: 5 of 5 by chance is 1/32).
            else if(d<-5.0&&pr<0.95&&nb*5>=np*4) v="FAIL (C more than 5% below B on warm TPC-B, in at least 80% of the paired rounds)"
            else v="INCONCLUSIVE (neither side of the 5% bar holds consistently across the medians and the paired rounds)"
            printf "- gate (warm TPC-B within 5%% of B): %s\n", v }
    }
    function relspread(cfg,key,   m){ m=median(cfg,key); if(m==""||m==0)return 0; return (hi(cfg,key)-lo(cfg,key))/m*100 }
    # paired ratios X/Y over the rounds where both ran: pr[] 1..k, sorted
    function pratios(M,x,y,pr,   r,k,i,j,t){ k=0; for(r in rounds_seen) if(((x,r) in M)&&((y,r) in M)) pr[++k]=M[x,r]/M[y,r]
        for(i=1;i<=k;i++)for(j=i+1;j<=k;j++)if(pr[j]<pr[i]){t=pr[i];pr[i]=pr[j];pr[j]=t}
        return k }
    function pcount(M,x,y,   pr){ return pratios(M,x,y,pr) }
    function pmed(M,x,y,   pr,k){ k=pratios(M,x,y,pr); if(k==0)return 0; return (k%2)?pr[(k+1)/2]:(pr[k/2]+pr[k/2+1])/2 }
    function pbelow(M,x,y,th,   pr,k,i,c){ k=pratios(M,x,y,pr); c=0; for(i=1;i<=k;i++)if(pr[i]<th)c++; return c }
    function pair(M,x,y,   pr,k){ k=pratios(M,x,y,pr); if(k==0)return ""; return sprintf("%.3f (%.3f–%.3f), n=%d", pmed(M,x,y), pr[1], pr[k], k) }
    ' "$RESULTS_JSON"
} >"$OUT"
log "wrote $OUT"
