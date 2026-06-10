# ZFS backend (experimental)

> **Status: experimental.** The zfs backend is unit-tested (command/plan
> construction, engine saga with a fake driver) and has a gated integration
> test (`PGBRANCH_ZFS_IT=1`), but it has **not** been exercised in CI on this
> project's development machine — Colima/macOS has no ZFS in the VM. This
> page contains everything needed to verify it by hand on a real Linux host.
> The default OverlayFS backend remains the supported path.

## Why a second backend

OverlayFS copies up **whole files** on first write, and Postgres heap/index
segments are files of up to 1 GiB — a branch that writes broadly converges on
a full copy of the dataset ([benchmarks](benchmarks.md)). ZFS does
copy-on-write at the **block** level: a clone shares blocks with its origin
snapshot and pays only for the blocks it actually changes, no matter which
file they live in. If you already run ZFS, the trade is usually worth it.

Two consequences worth knowing:

- ZFS clones have **no copy-up problem**, so the
  `recovery_init_sync_method=syncfs` flag the overlay entrypoint needs (see
  [benchmarks — The fix](benchmarks.md#the-fix)) isn't load-bearing here. The
  zfs entrypoint keeps it anyway for parity — it is simply harmless on ZFS.
- The branch entrypoint shrinks to perms + stale-pid cleanup + exec
  (`internal/cow/entrypoint_zfs.sh`): there is nothing to assemble, the clone
  *is* the writable data directory.

## How it works

With `branchd --cow zfs --zfs-dataset <prefix>`, layers become ZFS datasets
under `<prefix>` instead of docker/kube volumes:

| pgbranch object | overlay backend | zfs backend |
|---|---|---|
| source generation N | volume `pgbranch-src-<name>[-gN]` | dataset `<prefix>/src-<name>-gN` |
| branch writable layer | volume `pgbranch-br-<name>-rw` | dataset `<prefix>/br-<name>` (clone) |
| branch create | empty rw volume + in-container overlay mount | `zfs snapshot <src>@br-<name>` + `zfs clone` |
| branch destroy | remove rw volume | `zfs destroy -r` clone, then the snapshot |
| branch usage | `du -sb` on the rw volume | `zfs list -Hp -o used <clone>` |

- **Seeding** (`pg_basebackup`) targets the dataset's mountpoint, bind-mounted
  into the seed helpers. The backend assumes **default mountpoints**
  (`/<dataset>` — no `altroot`, no custom `mountpoint=`).
- **zfs commands run in privileged one-shot helpers** (`alpine:3.21`,
  `--privileged`, host `/dev/zfs` mapped in; on kube, a privileged pod). The
  helper installs the zfs userland at run time (`apk add zfs`), so it needs
  network access and an alpine `zfs` package version compatible with the
  host's zfs kernel module.
- **Branch containers** bind-mount the clone's mountpoint at `/pgbranch/rw`
  and run with `PGDATA=/pgbranch/rw/data`. No overlay assembly; WAL crash
  recovery on first boot, exactly as the overlay backend.
- **Destroys are idempotent**: an already-absent dataset/snapshot doesn't
  fail the destroy (so a half-created, failed branch stays destroyable), but
  a destroy that fails with the target still present — e.g. a busy clone —
  is surfaced as an error.

The registry stores dataset names in the same columns that hold volume names
on overlay. **Do not switch an existing `PGBRANCH_HOME` between backends** —
seed sources freshly under the backend you intend to use.

## Requirements

- Linux host where the **docker daemon** runs (the pool must be visible to
  the kernel that runs your containers — on macOS that means inside the VM,
  which Colima does not provide; this is why the IT is skipped there).
- An imported zpool and a dataset prefix pgbranch owns, e.g. `tank/pgbranch`.
- `/dev/zfs` present on the host (zfs kernel module loaded).
- Default mountpoints for everything under the prefix.
- Outbound network from helper containers (`apk add zfs`).

## Manual verification walkthrough

On a Linux box with docker and ZFS installed (a file-backed pool is fine for
testing):

```console
$ truncate -s 10G /var/tmp/pgbranch-pool.img
$ sudo zpool create tank /var/tmp/pgbranch-pool.img
$ sudo zfs create tank/pgbranch
$ ls -l /dev/zfs
crw-rw-rw- 1 root root 10, ... /dev/zfs
```

Start a demo source and branchd in zfs mode (same demo source as the README
quickstart):

```console
$ docker run -d --name demo-src -e POSTGRES_PASSWORD=secret postgres:17 \
    -c wal_level=replica -c max_wal_senders=4
$ docker exec demo-src sh -c 'until pg_isready -U postgres; do sleep 1; done'
$ docker exec demo-src psql -U postgres \
    -c "CREATE TABLE t(i int); INSERT INTO t SELECT generate_series(1,100000);"
$ docker exec demo-src sh -c \
    'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
$ docker exec demo-src psql -U postgres -c "SELECT pg_reload_conf();"
$ SRC_IP=$(docker inspect -f '{{.NetworkSettings.IPAddress}}' demo-src)

$ export PGBRANCH_TOKEN=$(openssl rand -hex 16)
$ ./bin/branchd --cow zfs --zfs-dataset tank/pgbranch
2026/06/10 12:00:00 REST API listening on :7070
```

Seed a source — expect a new `src-main-g1` dataset holding a `data/` dir:

```console
$ AUTH="Authorization: Bearer $PGBRANCH_TOKEN"
$ curl -H "$AUTH" -d "{\"name\":\"main\",\"host\":\"$SRC_IP\",\"port\":5432,
    \"user\":\"postgres\",\"password\":\"secret\"}" localhost:7070/v1/sources
{"name":"main","state":"ready",...}

$ zfs list -r tank/pgbranch
NAME                          USED  AVAIL  REFER  MOUNTPOINT
tank/pgbranch                  ...    ...    ...  /tank/pgbranch
tank/pgbranch/src-main-g1      ...    ...    ...  /tank/pgbranch/src-main-g1
$ sudo ls /tank/pgbranch/src-main-g1
data
```

Create a branch — expect a snapshot + clone and a near-instant create:

```console
$ curl -H "$AUTH" -d '{"name":"pr-1","source":"main"}' localhost:7070/v1/branches
{"name":"pr-1","state":"ready","port":<P>,...}

$ zfs list -r -t all tank/pgbranch
NAME                                  USED  AVAIL  REFER  MOUNTPOINT
tank/pgbranch/src-main-g1@br-pr-1       0      -    ...   -
tank/pgbranch/br-pr-1                 ...    ...    ...   /tank/pgbranch/br-pr-1
```

Verify isolation and usage (block-level CoW: the clone's `used` stays small
after small writes — no whole-segment copy-up):

```console
$ psql "host=localhost port=<P> user=postgres password=secret" \
    -c "DELETE FROM t WHERE i > 50000"
$ docker exec demo-src psql -U postgres -c "SELECT count(*) FROM t"   # still 100000

$ curl -H "$AUTH" localhost:7070/v1/branches/pr-1/usage
{"bytes":<N>}            # ≈ `zfs list -Hp -o used tank/pgbranch/br-pr-1`
```

Destroy — expect the clone and snapshot to be gone:

```console
$ curl -H "$AUTH" -X DELETE localhost:7070/v1/branches/pr-1
$ zfs list -r -t all tank/pgbranch
NAME                          USED  AVAIL  REFER  MOUNTPOINT
tank/pgbranch                  ...    ...    ...  /tank/pgbranch
tank/pgbranch/src-main-g1      ...    ...    ...  /tank/pgbranch/src-main-g1
```

The same flow is automated as `TestZFSEndToEndBranching`:

```console
$ PGBRANCH_ZFS_IT=1 PGBRANCH_ZFS_DATASET=tank/pgbranch \
    go test ./internal/engine/ -run TestZFSEndToEnd -count=1 -v
```

Cleanup: `zpool destroy tank && rm /var/tmp/pgbranch-pool.img`.

## Kubernetes note

The zfs backend follows the same **storage-node model** as overlay: the
zpool must live on the designated storage node, every zfs helper runs there
as a **privileged pod**, and branch pods mount the clone mountpoints via
`hostPath` (type `Directory` — a missing mountpoint fails the pod visibly
rather than starting on an empty dir). The Helm chart does not wire `--cow`
flags yet; run branchd with `--cow zfs --zfs-dataset ...` via a chart fork or
manual Deployment edit if you want to try it in-cluster.

## Known limitations

- Experimental: no CI coverage on real ZFS; verify with the walkthrough above.
- Helper containers install the zfs userland at run time (network required;
  alpine package vs host kernel-module compatibility is on you).
- `--cow` is a branchd flag; local-mode `pgb` (no `--server`) is overlay-only.
- One backend per `PGBRANCH_HOME` — don't mix.
- Branch-from-branch (clone of a clone) is future work, same as overlay.
