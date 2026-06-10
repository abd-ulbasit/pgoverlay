#!/bin/sh
# pgbranch branch entrypoint: assemble overlay CoW view of the source data
# dir, then hand off to the stock postgres entrypoint (WAL recovery runs there).
set -eu
: "${PGBRANCH_LOWERS:?}" "${PGDATA:?}"
mkdir -p /pgbranch/rw/upper /pgbranch/rw/work "$PGDATA"
mount -t overlay overlay \
  -o "lowerdir=${PGBRANCH_LOWERS},upperdir=/pgbranch/rw/upper,workdir=/pgbranch/rw/work" \
  "$PGDATA"
chown postgres:postgres "$PGDATA"
chmod 0700 "$PGDATA"
rm -f "$PGDATA/postmaster.pid"
# syncfs avoids per-file O_RDWR fsync during pre-recovery sync (which would force
# full OverlayFS copy-up); Linux-only and PG 14+, both guaranteed for branch containers.
exec docker-entrypoint.sh postgres -c recovery_init_sync_method=syncfs
