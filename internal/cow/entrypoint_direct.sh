#!/bin/sh
# pgbranch branch entrypoint, direct (zfs/csi) backends: the writable clone
# (ZFS clone dataset or CSI PVC clone) mounted at /pgbranch/rw is already a
# copy-on-write view of the source data dir — no overlay assembly. Fix perms,
# clear the stale postmaster.pid the clone inherited, and hand off to the
# stock postgres entrypoint (WAL crash recovery runs there).
set -eu
: "${PGDATA:?}"
chown postgres:postgres "$PGDATA"
chmod 0700 "$PGDATA"
rm -f "$PGDATA/postmaster.pid"
# recovery_init_sync_method=syncfs is kept for parity with the overlay
# entrypoint, where the default per-file fsync pass forces a full OverlayFS
# copy-up (see docs/benchmarks.md). Block/file-level clones have no copy-up
# problem — the flag is simply harmless here.
exec docker-entrypoint.sh postgres -c recovery_init_sync_method=syncfs
