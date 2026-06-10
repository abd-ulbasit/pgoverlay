# Benchmarks

Measured with [`hack/benchmark.sh`](https://github.com/abd-ulbasit/pgbranch/blob/main/hack/benchmark.sh) on 2026-06-10, against
pgbranch's Docker runtime with the OverlayFS copy-on-write backend. All numbers
are real, single-machine measurements — no extrapolation.

## Results

| Database size | pgbench scale | Seed time | Branch create (p50 of 5) | Branch rw overhead after create | rw after updating 1% of rows |
|---|---|---|---|---|---|
| 1.00 GiB (1,074,124,467 B) | 68 | 20 s | **1.90 s** | 33.1 MiB (34,748,732 B) | 1.04 GiB (1,113,471,292 B) |
| 5.00 GiB (5,370,484,403 B) | 342 | 41 s | **1.89 s** | 33.1 MiB (34,748,732 B) | 5.20 GiB (5,586,819,388 B) |

Individual branch-create runs (seconds):

- 1 GiB: 2.934, 2.323, 1.851, 1.901, 1.791 → p50 1.901
- 5 GiB: 2.062, 1.890, 1.796, 1.890, 1.918 → p50 1.890

Branch creation is now **independent of database size** — ~1.9 s p50 at both
1 GiB and 5 GiB — and a fresh branch costs 33.1 MiB of disk (recycled WAL
segments written during crash recovery plus overlay bookkeeping), not a copy
of the dataset. This is the copy-on-write behavior the design promises; it was
not true before 2026-06-10 (see [Before the fix](#before-the-fix-branch-creation-scaled-with-data-size)).

One number needs honest framing: the **rw layer after the 1% UPDATE probe** is
now ≈ the full dataset size, where the old table showed only +10–179 MiB.
The old probe was measuring growth of a layer that creation had *already*
copied up in full; the new probe pays the copy-up at first write instead.
OverlayFS copies up whole files, and Postgres heap/index segments are files of
up to 1 GiB — so a write that touches a segment copies that entire segment
into the rw layer, and this bulk UPDATE + CHECKPOINT ended up copying up
essentially the whole pgbench dataset. The cost is pay-per-file-written rather
than pay-at-create: branches that read mostly and write a little (the dev/test
case) stay thin; a branch that rewrites everything converges on ~1× the
database, exactly as before.

10 GiB was not run in this pass; the script's disk check skips any size that
doesn't fit (each size needs ~2.2× the target free in the Docker VM) and sizes
are configurable: `BENCH_SIZES_GIB="1 5 10" hack/benchmark.sh`.

## The fix

One flag. The branch entrypoint
([`internal/cow/entrypoint.sh`](https://github.com/abd-ulbasit/pgbranch/blob/main/internal/cow/entrypoint.sh)) now hands off
with:

```sh
exec docker-entrypoint.sh postgres -c recovery_init_sync_method=syncfs
```

Before replaying WAL, Postgres syncs the data directory
(`SyncDataDirectory`). With the default `recovery_init_sync_method=fsync` it
opens **every** data file read-write to fsync it — and on OverlayFS a
read-write open of a lower-layer file triggers a full copy-up, so the
pre-recovery sync pass alone copied the entire dataset into the branch's rw
layer. `syncfs` replaces the per-file pass with a single `syncfs()` call on
each filesystem containing data, which opens nothing read-write and copies
nothing up.

Why this is safe here: `syncfs` syncs the *whole filesystem* the data
directory lives on — a superset of what the per-file fsync pass covers — so
the durability guarantee the sync pass exists for (no dirty pages from before
recovery lingering unflushed) is preserved, and WAL crash-recovery semantics
are unchanged. The documented trade-offs of `syncfs` (errors on unrelated
files on the same filesystem can be reported, or I/O errors missed on some
kernels) are irrelevant for pgbranch branches: they are disposable dev/test
databases whose filesystem is the branch's own overlay mount. `syncfs` is
Linux-only and requires Postgres 14+; branch containers are always Linux and
always `postgres:14+`, which the entrypoint comments.

## Before the fix: branch creation scaled with data size

Everything in this section describes commit `2468425` and earlier — it is
kept because the diagnosis is the evidence for the fix. Same script, same
machine, 2026-06-10, before the entrypoint change:

| Database size | pgbench scale | Seed time | Branch create (p50 of 5) | Branch rw overhead after create | rw growth after updating 1% of rows |
|---|---|---|---|---|---|
| 1.00 GiB (1,074,124,467 B) | 68 | 8 s | **7.66 s** | 1.05 GiB (1,123,601,062 B) | +10.2 MiB (+10,665,984 B) |
| 5.00 GiB (5,370,484,403 B) | 342 | 29 s | **61.85 s** | 5.05 GiB (5,420,035,330 B) | +179.1 MiB (+187,850,752 B) |

Individual branch-create runs (seconds):

- 1 GiB: 7.710, 11.223, 7.658, 6.386, 6.382 → p50 7.658
- 5 GiB: 69.889, 66.125, 61.394, 59.976, 61.853 → p50 61.853

These numbers contradicted the naive expectation for a copy-on-write system
("creation time is independent of data size"), and the diagnosis was:

A branch starts as a stock `postgres:17` container whose `PGDATA` is an
OverlayFS mount: the seeded source volume read-only below, an empty rw volume
on top. Because the seed is a `pg_basebackup`, the branch's first boot is
crash recovery. **Before replaying WAL, Postgres fsyncs every file in the data
directory** (`SyncDataDirectory`, `recovery_init_sync_method=fsync`, the
default), and it opens each regular file read-write to do so. On OverlayFS, a
read-write open of a lower-layer file triggers a full copy-up — so this
pre-recovery sync pass copied the entire dataset into the branch's rw layer.
The WAL replay itself is trivial (`redo done ... elapsed: 0.00 s` in the
branch logs); essentially all of the create time was this copy-up — ~140 MB/s
for the 1 GiB dataset (which fits the VM's page cache) and ~87 MB/s for the
5 GiB one (which doesn't).

Two measurements pinned the mechanism down:

- The rw layer right after creation was ≈ the full database size (1.05 GiB for
  a 1.00 GiB database; 5.05 GiB for a 5.00 GiB one — the excess over the
  database size is recycled WAL segments and overlay bookkeeping).
- A control run identical except for `-c recovery_init_sync_method=syncfs`
  (one `syncfs()` call instead of per-file fsync) finished recovery with the
  rw layer at **16 KiB** — no copy-up at all.

That control run is exactly the fix that now ships; the
[Results](#results) table above is the same benchmark re-run with it in place.
The old write-amplification numbers (+10.2 MiB / +179.1 MiB for a 1% UPDATE)
measured growth of an already-fully-copied-up rw layer — WAL plus dirtied heap
pages only — and are not comparable to the post-fix probe, which pays
whole-file copy-up at first write (see the note under Results).

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
reports the resulting rw size.

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
  Linux with local NVMe will be faster across the board. (Pre-fix, the
  copy-up-bound create times tracked the VM's disk throughput — ~87 MB/s
  effective at 5 GiB; post-fix, create time is no longer disk-bound.)
- **virtiofs is not in the data path.** Colima mounts the macOS home over
  virtiofs, but all measured I/O hits Docker named volumes on the VM-local
  ext4 disk — these numbers do not include virtiofs penalties (nor its
  cache benefits).
- **`du -sb` is apparent size**, not allocated blocks; sparse files (rare in
  `PGDATA`) would overcount, and overlay/ext4 metadata is not charged.
- **Seed time has 1 s resolution** (wall-clock around the CLI call); branch
  create times come from the CLI's own millisecond-rounded duration. Seed
  times are not comparable between the before/after tables (different cache
  state, same mechanism).
- **Postgres runs with stock image defaults** (128 MB `shared_buffers`, 1 GB
  `max_wal_size`); a tuned source would seed faster.
- An idle kind (Kubernetes-in-Docker) control-plane container from another
  test setup was present in the same VM during the runs; it was not serving
  any workload.
