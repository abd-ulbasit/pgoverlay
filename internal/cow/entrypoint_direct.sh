#!/bin/sh
# pgoverlay branch entrypoint, direct (zfs/csi) backends: the writable clone
# (ZFS clone dataset or CSI PVC clone) mounted at /pgoverlay/rw is already a
# copy-on-write view of the source data dir — no overlay assembly. Fix perms,
# clear the stale postmaster.pid the clone inherited, and hand off to the
# stock postgres entrypoint (WAL crash recovery runs there).
set -eu
: "${PGDATA:?}"
chown postgres:postgres "$PGDATA"
chmod 0700 "$PGDATA"
rm -f "$PGDATA/postmaster.pid"
# extra postgres settings, appended after the fixed ones at the end
set --
# --- shared with entrypoint.sh from here on; keep the two in sync ---
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
# recovery_init_sync_method=syncfs is kept for parity with the overlay
# entrypoint, where the default per-file fsync pass forces a full OverlayFS
# copy-up (see docs/benchmarks.md). Block/file-level clones have no copy-up
# problem — the flag is simply harmless here.
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
