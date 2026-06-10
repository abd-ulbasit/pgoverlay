# Benchmarks

Measured with [`hack/benchmark.sh`](../hack/benchmark.sh) on 2026-06-10, against
pgbranch's Docker runtime with the OverlayFS copy-on-write backend. All numbers
are real, single-machine measurements — no extrapolation.

## Results

| Database size | pgbench scale | Seed time | Branch create (p50 of 5) | Branch rw overhead after create | rw growth after updating 1% of rows |
|---|---|---|---|---|---|
| 1.00 GiB (1,074,124,467 B) | 68 | 8 s | **7.66 s** | 1.05 GiB (1,123,601,062 B) | +10.2 MiB (+10,665,984 B) |
| 5.00 GiB (5,370,484,403 B) | 342 | 29 s | **61.85 s** | 5.05 GiB (5,420,035,330 B) | +179.1 MiB (+187,850,752 B) |

Individual branch-create runs (seconds):

- 1 GiB: 7.710, 11.223, 7.658, 6.386, 6.382 → p50 7.658
- 5 GiB: 69.889, 66.125, 61.394, 59.976, 61.853 → p50 61.853

10 GiB was not run in this pass; the script's disk check skips any size that
doesn't fit (each size needs ~2.2× the target free in the Docker VM) and sizes
are configurable: `BENCH_SIZES_GIB="1 5 10" hack/benchmark.sh`.

## The honest headline: branch creation currently scales with data size

The numbers above contradict the naive expectation for a copy-on-write system
("creation time is independent of data size"), and it's worth being precise
about why.

A branch starts as a stock `postgres:17` container whose `PGDATA` is an
OverlayFS mount: the seeded source volume read-only below, an empty rw volume
on top. Because the seed is a `pg_basebackup`, the branch's first boot is
crash recovery. **Before replaying WAL, Postgres fsyncs every file in the data
directory** (`SyncDataDirectory`, `recovery_init_sync_method=fsync`, the
default), and it opens each regular file read-write to do so. On OverlayFS, a
read-write open of a lower-layer file triggers a full copy-up — so this
pre-recovery sync pass copies the entire dataset into the branch's rw layer.
The WAL replay itself is trivial (`redo done ... elapsed: 0.00 s` in the
branch logs); essentially all of the create time is this copy-up — ~140 MB/s
for the 1 GiB dataset (which fits the VM's page cache) and ~87 MB/s for the
5 GiB one (which doesn't).

Two measurements pin the mechanism down:

- The rw layer right after creation is ≈ the full database size (1.05 GiB for
  a 1.00 GiB database; 5.05 GiB for a 5.00 GiB one — the excess over the
  database size is recycled WAL segments and overlay bookkeeping).
- A control run identical except for `-c recovery_init_sync_method=syncfs`
  (one `syncfs()` call instead of per-file fsync) finished recovery with the
  rw layer at **16 KiB** — no copy-up at all.

Consequences, honestly stated:

- **Create time** grows with database size on the overlay backend: 7.66 s at
  1 GiB vs 61.85 s at 5 GiB (p50) — slightly super-linear here because the
  larger dataset no longer fits the VM's page cache.
- **Disk per branch** is currently ~1× the database, not "only changed pages",
  until this is fixed. Branches still share nothing logically — writes are
  isolated and destroy/reset are instant — but the thin-clone disk economy is
  not realized today.
- The fix is known and small (start branch Postgres with
  `recovery_init_sync_method=syncfs`, available since Postgres 14): the syncfs
  control run shows it eliminates the copy-up. It is not yet wired into the
  engine, so these benchmarks report the system as it ships.

The write-amplification probe (`UPDATE` on 1% of `pgbench_accounts` rows,
then `CHECKPOINT`) measures growth of an already-copied-up rw layer, so it
mostly reflects WAL plus dirtied heap pages: +10.2 MiB for 68,000 updated rows
(1 GiB), +179.1 MiB for 342,000 rows (5 GiB).

## Methodology

Everything below is what `hack/benchmark.sh` does; run it yourself with
`make build && hack/benchmark.sh`.

**Source database.** A throwaway `postgres:17` container with
`-c wal_level=replica -c max_wal_senders=4` and a
`host replication all all scram-sha-256` line appended to `pg_hba.conf`
(the stock image has no remote replication entry). Data is generated with
`pgbench -i -q -s <scale>` followed by `CHECKPOINT`; the size reported is
`pg_database_size(current_database())`.

**Scale calibration.** A scale-10 database is initialized first and
bytes-per-scale derived empirically: an empty database is 7,689,907 B, scale
10 is 164,665,011 B → **15,697,510 B (≈15.0 MiB) per scale unit** including
indexes. Target scales are computed from that: 68 → 1.00 GiB, 342 → 5.00 GiB.

**Seed time.** Wall time of `pgb source add` (1 s resolution), which runs
`pg_basebackup` from the source container into the copy-on-write source
volume.

**Branch create p50.** Five cycles of
`pgb branch create benchwork --from benchsrc` / `pgb branch destroy benchwork`;
each time is parsed from the CLI's `ready in <duration>` line (the CLI stops
the clock when Postgres in the branch accepts connections). p50 is the median
of the five.

**Branch rw overhead.** `du -sb` of the branch's rw volume (upper + work
dirs) via a one-shot `alpine:3.21` container, immediately after create — the
same measurement `GET /v1/branches/{name}/usage` and `pgb branch ls --usage`
perform.

**Write amplification.** On the still-running branch:
`UPDATE pgbench_accounts SET abalance = abalance + 1 WHERE aid <= <scale*1000>`
(exactly 1% of rows), then `CHECKPOINT`, then the same `du -sb`; the table
reports the delta.

**Disk check.** Free space is read from `df -Pk /` inside a one-shot
container (a container's root is an overlay on the filesystem Docker keeps
volumes on); sizes needing more than ~2.2× target + 2 GiB margin are skipped.

## Hardware

- **Host:** Apple M1 Pro, 16 GiB RAM, macOS 26.5.1.
- **VM:** Colima (macOS Virtualization.Framework), aarch64, 4 CPUs, 8 GiB
  RAM, Ubuntu 24.04.2, kernel 6.8.0-64-generic, Docker 28.4.0 (overlayfs
  storage driver). Docker volumes live on a VM-local ext4 data disk
  (`/dev/vdb1`, 98 GiB, mounted at `/var/lib/docker`).
- **Images:** `postgres:17` (17.10) for source and branches, `alpine:3.21`
  for measurement helpers.

## Caveats

- **Virtualization overhead.** Everything runs inside a Colima VM; bare-metal
  Linux with local NVMe will be faster across the board, and the
  copy-up-bound create times in particular track the VM's disk throughput
  (~87 MB/s effective at 5 GiB).
- **virtiofs is not in the data path.** Colima mounts the macOS home over
  virtiofs, but all measured I/O hits Docker named volumes on the VM-local
  ext4 disk — these numbers do not include virtiofs penalties (nor its
  cache benefits).
- **`du -sb` is apparent size**, not allocated blocks; sparse files (rare in
  `PGDATA`) would overcount, and overlay/ext4 metadata is not charged.
- **Seed time has 1 s resolution** (wall-clock around the CLI call); branch
  create times come from the CLI's own millisecond-rounded duration.
- **Postgres runs with stock image defaults** (128 MB `shared_buffers`, 1 GB
  `max_wal_size`); a tuned source would seed faster.
- An idle kind (Kubernetes-in-Docker) control-plane container from another
  test setup was present in the same VM during the runs; it was not serving
  any workload.
