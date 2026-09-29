#!/bin/sh
# pgoverlay branch entrypoint: assemble overlay CoW view of the source data
# dir, then hand off to the stock postgres entrypoint (WAL recovery runs there).
set -eu
: "${PGOVERLAY_LOWERS:?}" "${PGDATA:?}"
mkdir -p /pgoverlay/rw/upper /pgoverlay/rw/work "$PGDATA"
mount -t overlay overlay \
  -o "lowerdir=${PGOVERLAY_LOWERS},upperdir=/pgoverlay/rw/upper,workdir=/pgoverlay/rw/work" \
  "$PGDATA"
chown postgres:postgres "$PGDATA"
chmod 0700 "$PGDATA"
rm -f "$PGDATA/postmaster.pid"
# extra postgres settings, appended after the fixed ones at the end
set --

# --- lazyrw: copy a relation file up on its first write, not on open ---
# Postgres opens every relation file O_RDWR, even to read it, and OverlayFS
# copies a lower file up in full on a read-write open, so a read-only query
# would copy whole tables into the branch. The lazyrw preload (installed in
# /pgoverlay/rw/lazyrw by branchd, source in internal/cow/lazyrw) opens them
# read-only and reopens a file read-write on its first write. It is used only
# when this kernel re-targets read-only fds after a copy-up (the self-test
# below) and a build for this image's libc and architecture loads into its
# postgres (the probe below). Otherwise the branch copies eagerly, as without
# the shim, and says so. /pgoverlay/rw/cow-mode records the outcome for
# branchd: "lazyrw", "eager" or "off" on the first line, a detail on the
# second.
lazyrw_dir=/pgoverlay/rw/lazyrw
lazyrw_selftest_dir=/pgoverlay/rw/.selftest
lazyrw_lower=pgoverlay-lazyrw-selftest-lower
lazyrw_upper=pgoverlay-lazyrw-selftest-upper

# lazyrw_selftest proves what the shim's correctness rests on: a file opened
# read-only before another open copies it up and writes it must then read the
# new data. OverlayFS does that since Linux 4.19 (stacked file operations);
# before, the old fd keeps reading the lower file, and a backend holding a
# read-only fd would read stale pages after another backend's first write.
# The group's stdin is that read-only fd: it is opened before the group runs.
lazyrw_selftest() {
  st=$lazyrw_selftest_dir
  rm -rf "$st" || :
  mkdir -p "$st/lower" "$st/upper" "$st/work" "$st/merged" || return 1
  printf %s "$lazyrw_lower" > "$st/lower/f" || return 1
  mount -t overlay overlay -o "lowerdir=$st/lower,upperdir=$st/upper,workdir=$st/work" "$st/merged" || return 1
  got=$({ printf %s "$lazyrw_upper" 1<>"$st/merged/f" && cat; } <"$st/merged/f") || got=
  umount "$st/merged" || :
  rm -rf "$st" || :
  [ "$got" = "$lazyrw_upper" ]
}

# lazyrw_oneline joins the first lines of $1 into one line for cow-mode.
lazyrw_oneline() {
  printf '%s\n' "$1" | head -n 3 | tr '\n' ' ' | sed 's/ *$//'
}

# lazyrw_probe starts `postgres -V` with the preload list $1, as the postgres
# user where the image has gosu or su-exec (the stock entrypoint drops to it
# the same way), and succeeds only when the shim reports itself active: a
# preload that does not load is ignored silently by musl and with an error
# line by glibc, so an empty stderr would prove nothing. On failure it leaves
# the reason in lazyrw_why.
lazyrw_probe() {
  runas=
  if command -v gosu >/dev/null 2>&1; then
    runas="gosu postgres"
  elif command -v su-exec >/dev/null 2>&1; then
    runas="su-exec postgres"
  fi
  # $runas is split into the command and its user on purpose
  # shellcheck disable=SC2086
  out=$(PGOVERLAY_LAZYRW_DEBUG=1 LD_PRELOAD=$1 $runas postgres -V 2>&1 >/dev/null) || {
    lazyrw_why="postgres -V with $1 preloaded failed: $(lazyrw_oneline "$out")"
    return 1
  }
  if ! printf '%s\n' "$out" | grep -q '^\[lazyrw\] active '; then
    lazyrw_why="$1 did not load into postgres${out:+: $(lazyrw_oneline "$out")}"
    return 1
  fi
  if printf '%s\n' "$out" | grep -qv '^\[lazyrw\]'; then
    lazyrw_why="postgres -V with $1 preloaded wrote to stderr: $(lazyrw_oneline "$(printf '%s\n' "$out" | grep -v '^\[lazyrw\]')")"
    return 1
  fi
}

# lazyrw_enable picks the build for this image (musl first when its loader is
# present, then glibc), checks it and exports LD_PRELOAD. It sets cow_mode and
# cow_detail.
lazyrw_enable() {
  cow_mode=eager
  if ! lazyrw_selftest; then
    cow_detail="self-test failed: a file opened read-only before a copy-up did not read the data written after it (the kernel needs OverlayFS stacked file operations, Linux 4.19 or later)"
    return 0
  fi
  arch=$(uname -m) || arch=unknown
  libcs="glibc musl"
  for f in /lib/ld-musl-*; do
    if [ -e "$f" ]; then libcs="musl glibc"; fi
    break
  done
  cow_detail=
  for libc in $libcs; do
    so=$lazyrw_dir/liblazyrw-$libc-$arch.so
    if [ ! -f "$so" ]; then
      lazyrw_why="no lazyrw build for $libc-$arch in $lazyrw_dir"
    elif lazyrw_probe "$so${LD_PRELOAD:+:$LD_PRELOAD}"; then
      export LD_PRELOAD="$so${LD_PRELOAD:+:$LD_PRELOAD}"
      cow_mode=lazyrw
      cow_detail=$so
      return 0
    fi
    # report why the preferred build failed, not the fallback
    cow_detail=${cow_detail:-$lazyrw_why}
  done
}

cow_mode=off
cow_detail="PGOVERLAY_LAZYRW=off"
if [ "${PGOVERLAY_LAZYRW:-on}" != off ]; then
  lazyrw_enable
fi
printf '%s\n%s\n' "$cow_mode" "$cow_detail" > /pgoverlay/rw/cow-mode
case $cow_mode in
  lazyrw) echo "pgoverlay: lazyrw active ($cow_detail): reads copy nothing; a file is copied into the branch on its first write" >&2 ;;
  eager) echo "pgoverlay: WARN: lazyrw is not active, so every relation file Postgres opens is copied into the branch: $cow_detail" >&2 ;;
esac

# pg_major prints the cluster's major version (PG_VERSION, else the image's
# PG_MAJOR, else postgres -V), or 0 when none of them says.
pg_major() {
  v=$(cat "$PGDATA/PG_VERSION" 2>/dev/null) || v=
  case $v in '' | *[!0-9]*) v=${PG_MAJOR:-} ;; esac
  case $v in '' | *[!0-9]*) v=$(postgres -V 2>/dev/null | sed -n 's/^[^0-9]*\([0-9][0-9]*\).*/\1/p') || v= ;; esac
  case $v in '' | *[!0-9]*) v=0 ;; esac
  echo "$v"
}
# PG 18 reads through io_uring when io_method=io_uring is configured, and the
# shim sees libc calls, not io_uring submissions: while the shim is active the
# branch pins the worker method (18's default), which does its I/O through
# libc. Settings on the command line override postgresql.conf and ALTER SYSTEM.
if [ "$cow_mode" = lazyrw ] && [ "$(pg_major)" -ge 18 ]; then
  set -- "$@" -c io_method=worker
fi
# Experimental (branchd --wal-recycle=off): recycling a WAL segment renames it,
# and renaming a segment that came from the seed copies it up first.
if [ "${PGOVERLAY_WAL_RECYCLE:-on}" = off ]; then
  set -- "$@" -c wal_recycle=off
fi

# --- shared with entrypoint_direct.sh from here on; keep the two in sync ---
# A branch is always an independent, writable primary. A base backup of a
# standby carries standby.signal; seeding removes it, this covers sources
# seeded before it did.
rm -f "$PGDATA/standby.signal" "$PGDATA/recovery.signal"
# Distro-packaged sources (Debian/Ubuntu) keep their config files outside
# the data dir, so pg_basebackup copies none and postgres refuses to start.
# An empty postgresql.conf is valid; the hba matches dump-seeded sources
# (md5 also accepts scram-sha-256 verifiers).
for f in postgresql.conf pg_ident.conf; do
  [ -e "$PGDATA/$f" ] || { : > "$PGDATA/$f"; chown postgres:postgres "$PGDATA/$f"; }
done
if [ ! -e "$PGDATA/pg_hba.conf" ]; then
  printf '%s\n' 'local all all trust' 'host all all all md5' > "$PGDATA/pg_hba.conf"
  chown postgres:postgres "$PGDATA/pg_hba.conf"
fi
# ssl=on with certificates outside the data dir cannot start here.
[ -e "$PGDATA/server.crt" ] || set -- "$@" -c ssl=off
# syncfs avoids per-file O_RDWR fsync during pre-recovery sync (which would force
# full OverlayFS copy-up); Linux-only and PG 14+, both guaranteed for branch containers.
# The rest overrides what the source's config would otherwise impose: the
# port, sockets and config paths the container and the readiness probe
# expect, and no replication, WAL archiving or synchronous-standby wait
# pointing back at the source (primary_conninfo can carry its password).
exec docker-entrypoint.sh postgres -c recovery_init_sync_method=syncfs \
  -c port=5432 -c "listen_addresses=*" -c unix_socket_directories=/var/run/postgresql \
  -c "hba_file=$PGDATA/pg_hba.conf" -c "ident_file=$PGDATA/pg_ident.conf" \
  -c logging_collector=off \
  -c primary_conninfo= -c primary_slot_name= -c restore_command= \
  -c archive_cleanup_command= -c recovery_end_command= \
  -c archive_mode=off -c synchronous_standby_names= \
  "$@"
