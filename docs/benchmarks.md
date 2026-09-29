# Benchmarks

Measured with [`hack/benchmark.sh`](https://github.com/abd-ulbasit/pgoverlay/blob/main/hack/benchmark.sh) on 2026-06-10, against
pgoverlay's Docker runtime with the OverlayFS copy-on-write backend. All numbers
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
of the dataset. That was not true before 2026-06-10 (see
[Before the fix](#before-the-fix-branch-creation-scaled-with-data-size)).
What a branch costs after it starts serving queries is a different number:
see [Reads copy up too](#reads-copy-up-too) for how it was before v1.0.0, and
[True copy-on-write (v1.0.0)](#true-copy-on-write-v100) for how it is now.

One number needs honest framing: the **rw layer after the 1% UPDATE probe** is
now ≈ the full dataset size, where the old table showed only +10–179 MiB.
The old probe was measuring growth of a layer that creation had *already*
copied up in full; the new probe pays the copy-up at first write instead.
OverlayFS copies up whole files, and Postgres heap/index segments are files of
up to 1 GiB — so a write that touches a segment copies that entire segment
into the rw layer, and this bulk UPDATE + CHECKPOINT ended up copying up
essentially the whole pgbench dataset. The cost moved from create time to
first use, and, as the next section shows, before v1.0.0 "use" included
reads: a branch converged on the size of the tables it touched, not only the
ones it wrote. (This table predates v1.0.0's seed settle and lazyrw shim.
Writes still copy whole segments on ext4, so the 1% UPDATE column would not
change much there; on XFS or btrfs it would be a fraction of it.)

10 GiB was not run in this pass; the script's disk check skips any size that
doesn't fit (each size needs ~2.2× the target free in the Docker VM) and sizes
are configurable: `BENCH_SIZES_GIB="1 5 10" hack/benchmark.sh`.

## Reads copy up too

**Fixed in v1.0.0** for the default setup: see
[True copy-on-write (v1.0.0)](#true-copy-on-write-v100). This section is kept
as it was measured, because it is the evidence for that work, and because a
branch whose kernel or image cannot run the lazyrw shim (eager mode) still
behaves this way.

An earlier version of this page said that branches which mostly read stay
thin. That was wrong, and the pre-v1 review measured why (issue
[#13](https://github.com/abd-ulbasit/pgoverlay/issues/13)). Setup: commit
`c55cab2`, Docker Engine 29.6.2 on Linux amd64 (ext4), PostgreSQL 17, a
489 MB table that was `VACUUM (FREEZE)`d on the source before seeding, so
62,500 of its 62,528 pages were all-frozen and setting hint bits could not
explain any growth. The rw layer was measured with the same `du -sb` helper as
above.

| Branch state | rw layer |
|---|---|
| fresh branch | 33.1 MiB |
| after a 1-row `UPDATE` on a small table | 33.5 MiB |
| after `SELECT count(*)` on a tiny table + `CHECKPOINT` | 34.6 MiB |
| right after `SELECT count(*)` on the 489 MB table, before any checkpoint | **523.1 MiB** |

The file that appeared in the rw layer, `base/5/16384`, was 488.5 MiB: the
whole table, copied by one read-only query.

**Why.** It is the `SyncDataDirectory` mechanism from [The fix](#the-fix),
on the normal read path. PostgreSQL's storage manager opens every relation
segment read-write, whatever the query does: `src/backend/storage/smgr/md.c`
opens with `O_RDWR | PG_BINARY` in `REL_14_STABLE` (line 494), and through
`_mdfd_open_flags()` (line 146) in `REL_17_STABLE`. OverlayFS decides on
copy-up at `open()` time, from the open flags, before any byte is written: a
read-write open of a file that exists only in a lower layer copies the whole
file into the upper layer first. So the first query of any kind that touches
a table copies each segment it opens (up to 1 GiB per segment), and pays for
that copy inline. A small table costs almost nothing, which is why the 1-row
`UPDATE` above barely moved the layer.

**What it means.**

- A fresh branch still costs about 33 MiB and still starts in about 2 s;
  creation is unaffected.
- A branch grows by the size of every table and index file it opens, read or
  write, up to roughly the size of the data it touches. A test suite that
  scans a few large tables will copy those tables once per branch.
- The first query to touch a large table is slow on a new branch, because it
  waits for the copy.
- `syncfs` cannot help here: the open flags are how stock Postgres reads.
  Avoiding the copy needs either a filesystem that clones instead of copying,
  or changing the flags Postgres's opens reach the kernel with, which is what
  v1.0.0 does.

**What the advice was.** Before v1.0.0 this page pointed read-heavy branches
of large databases at the block-level backends ([zfs](zfs.md), Kubernetes
[csi mode](kubernetes.md#recommended-csi-mode)). Since v1.0.0 the overlay
backend reads without copying, so that advice now applies only to branches
that write into many large tables on a host whose volumes cannot clone; and
a branch reported as `eager` by `pgoverlay_branch_cow_mode` should be fixed
([Troubleshooting](troubleshooting.md#a-branch-copies-eagerly)) rather than
sized for.

## True copy-on-write (v1.0.0)

Issue [#49](https://github.com/abd-ulbasit/pgoverlay/issues/49) set the bar:
on the default setup (plain Docker on an ext4 host, stock `postgres:14` to
`18` images) a read-only workload adds approximately nothing to a branch, a
write copies as little as the mechanism allows, create time stays
independent of database size, and no new privileges.

### What changed

- **The lazyrw shim, on by default** on the overlay backend (Docker and
  Kubernetes hostPath). An `LD_PRELOAD` library in the branch's Postgres opens
  table files read-only and reopens a file read-write on its first write,
  which is when OverlayFS copies it
  ([how it works](concepts.md#copy-on-first-write-the-lazyrw-shim)).
- **Seed settle, on by default.** Each new seed is recovered,
  `VACUUM (FREEZE, ANALYZE)`d and cleanly shut down once, so branches start
  without WAL replay and their reads have no hint bits to set
  ([how it works](concepts.md#seed-settle-doing-the-first-reads-writes-once)).
- **Block level where the filesystem allows it.** Where the volumes sit on
  XFS (`reflink=1`) or btrfs, OverlayFS copy-up is an extent clone. branchd
  detects it, counts usage as exclusive bytes, and `--volume-root` puts the
  volumes on such a disk on any Docker host
  ([how it works](concepts.md#8-clone-or-copy-what-a-copy-up-costs)).

### The evaluation

Every option in #49 was measured on one host before anything was built:
Linux 7.0 (amd64), Docker 29.6.2 with volumes on ext4, PostgreSQL 17, and a
table of about 500 MB (1.5M rows, 488 MB of heap plus a 32 MB primary key)
frozen on the source. Branches started with crash recovery, as they did
then; latencies are with a warm page cache, so the first query's extra time
is the copy.

| Mechanism | `SELECT count(*)` adds | First query | A write copies | Verdict |
|---|---|---|---|---|
| OverlayFS on ext4 (before v1.0.0) | +555 MiB | 20.8 s | the whole file, on open | replaced |
| OverlayFS with `metacopy=on` | the same bytes | 11.3 s | the whole file, on open | rejected: the kernel copies data on any write-mode open |
| fuse-overlayfs | the same bytes | 15.8 s | the whole file, on open | rejected |
| **lazyrw shim + OverlayFS, ext4** | **+0 bytes** | **277 ms** | the touched segment (up to 1 GiB), once | **default** |
| OverlayFS on XFS `reflink=1` or btrfs | ~0 | ~10 ms per file | blocks | **automatic where the volumes reflink** |
| Managed loopback XFS pool on ext4 | ~0 | 208 ms | blocks | deferred: fsync ran at 0.4 to 0.6x of ext4, and it needs a privileged loop device lifecycle |
| btrfs snapshots, dm-thin, qcow2 over nbd | ~0 | about 0.2 s | blocks | rejected: privileged, fragile set-up, extra kernel modules, the slowest fsync |
| Per-file `FICLONE` backend, no overlay | 0 | | blocks | later: needs a reflink filesystem, and create time grows with the file count |

The raw OverlayFS numbers behind the first rows (bytes of the branch's upper
directory, `du -sb`): 34,735,718 B fresh, 582,461,030 B after the
`SELECT count(*)`, and byte for byte the same with `metacopy=on` and with
fuse-overlayfs; the first `SELECT` took 19.4 s and the second 116 ms in that
run. With the shim the upper directory stayed at 16.2 MiB across the read.

Two findings from the evaluation shaped the implementation:

- **An unsettled seed still copies on read.** With a seed that was not
  frozen, the shim's first `SELECT` still copied 68 MiB (PG 14 and 17) and
  119 MiB (PG 18): reading rows sets their hint bits, which is a real write.
  pgoverlay's `pg_basebackup` seed is exactly such a copy, hence seed settle.
- **Select-only throughput is unchanged.** `pgbench -S` gave 27,511 tps in a
  branch with the shim and 27,007 tps on plain Postgres on ext4. TPC-B numbers from that host were not
  usable: it was shared with other workloads, and plain ext4 TPC-B fell from
  925 to 122 tps within 40 minutes. The quiet-host run below replaces them.
  `pg_test_fsync` (8 kB write plus `fdatasync`, interleaved) is the
  comparison that held: 670/757/621 ops/s on ext4, 429/275/251 on XFS on a
  loop device, 170/117/95 on btrfs on a loop device, which is why the loop
  pool is deferred.

### Measured on the implementation

The integrated tree (`v1-cow` at `65372f9`), built with `make build` and run
with default settings on the same host: a `postgres:17` source with a
200 MiB table that was never vacuumed, seeded with `--seed-settle=freeze`,
once with Docker's own volumes (ext4) and once with `--volume-root` on an
XFS `reflink=1` disk. Branch usage is what `pgb branch ls --usage` reports.

| Check | ext4 (Docker volumes) | XFS `--volume-root` |
|---|---|---|
| seed including settle | 22.8 s (settle 6.6 s) | 17.9 s (settle 6.2 s) |
| branch create | 2.9 s | 3.5 s |
| copy-up probe (`pgoverlay_cow_copyup_mode`) | `copy` | `clone`, 16 KiB `cowextsize` hint set |
| `cow-mode` | `lazyrw` | `lazyrw` |
| seq scan of the table after settle | 25,408 pages read, 0 dirtied | same |
| first read pass (count, `EXPLAIN`, `pg_relation_size`, full read), then `CHECKPOINT` | data +0.24 MiB, WAL +16 MiB | same |
| second read pass | +0 bytes | +0 bytes |
| 1-row `UPDATE` of a small table | +0.08 MiB | +0.08 MiB |
| 1-row `UPDATE` of the 200 MiB table | +200 MiB (its segment, once) | +0.02 MiB exclusive (`du -sb`: +200 MiB) |
| the same read with `--lazyrw=off` | +201.5 MiB, `cow-mode` `off` | same |

What the first read pass still copies is constant, not proportional to the
data read: 224 KiB of `pg_statistic` (hint bits on the rows `ANALYZE` wrote
after `pg_statistic` itself was vacuumed) plus 8 KiB each of `pg_xact`,
`pg_subtrans` and `pg_multixact`, and the current 16 MiB WAL segment the
first time the branch writes WAL (a clone on XFS). Reset, branch-from-branch
(the child reads the parent's writes; the parent does not see the child's)
and destroy behaved the same in both runs.

Other measurements from the pull requests that built it (same host):

- a read of a frozen 196 MB table in a branch created by the engine: +0 bytes
  in about 200 ms with the shim, +207 MB in 3.5 s with `--lazyrw=off`
  ([#53](https://github.com/abd-ulbasit/pgoverlay/pull/53)); PG 18 and a
  `postgres:17-alpine` (musl) branch also came up `lazyrw` with reads adding
  0 bytes;
- a 1-row `UPDATE` of a ~56 MiB table: +58 MiB with Docker's volumes on ext4,
  +44 KiB of exclusive bytes with `--volume-root` on XFS `reflink=1` and on
  btrfs ([#51](https://github.com/abd-ulbasit/pgoverlay/pull/51));
- a branch of a settled seed starts with no redo, and a sequential scan of a
  never-vacuumed table dirties 0 buffers, on PG 14, 16-alpine, 17 and 18
  ([#52](https://github.com/abd-ulbasit/pgoverlay/pull/52)).

### Throughput and the first-write stall

<!-- PLACEHOLDER: the coordinator fills this table from the #49 benchmark run and removes this comment and the note below. -->

**Placeholder: these numbers are not measured yet.** They come from the
quiet-host benchmark run for #49 and will replace the cells marked TBD.

| | `pgbench -S` (tps) | TPC-B (tps) | first write into a cold 1 GiB segment | read of a large frozen table adds |
|---|---|---|---|---|
| plain Postgres on ext4, no overlay | TBD | TBD | n/a | n/a |
| branch, `--lazyrw=off` (eager), ext4 | TBD | TBD | TBD | TBD |
| branch, lazyrw (default), ext4 | TBD | TBD | TBD | TBD |
| branch, lazyrw, `--volume-root` on XFS `reflink=1` | TBD | TBD | TBD | TBD |

Method: interleaved A/B/A runs, three of each, on a host with no other
workload; `pgbench -S` and TPC-B at scale 50 with `-c 8 -j 4 -T 60`, after the
segments the benchmark writes have been copied once. The release gate for
v1.0.0 is select-only throughput within noise of the eager branch, and TPC-B
within 5% of it.

### Not measured

Every number above is from linux/amd64 on kernel 7.0. arm64 runs in CI (the
shim's C tests and the copy-on-write integration tests, on
`ubuntu-24.04-arm`). Docker Desktop, Colima and OrbStack run recent kernels
and arm64 or amd64 images, so the shim is expected to work there, but they
have not been measured; Kubernetes hostPath runs the same entrypoint and
install path as Docker and was not measured either.

## The fix

One flag. The branch entrypoint
([`internal/cow/entrypoint.sh`](https://github.com/abd-ulbasit/pgoverlay/blob/main/internal/cow/entrypoint.sh)) now hands off
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
kernels) are irrelevant for pgoverlay branches: they are disposable dev/test
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

## Branch-from-branch

Measured 2026-06-11 (Phase 5), one quick pass at ~1 GiB: a 1.12 GiB source
(1M rows × ~1 KB, `pg_database_size` = 1,200,805,555 B), branch `b1` created,
1% of rows updated on `b1` + `CHECKPOINT`, then `b2` created **from `b1`**
three times (create/destroy cycles):

| Database size | create-from-branch (p50 of 3) | individual runs |
|---|---|---|
| 1.12 GiB | **4.79 s** | 8.36 s, 4.79 s, 3.86 s |

Branch-from-branch on the overlay backend is a *freeze*: the parent is
checkpointed and stopped, its rw volume becomes an immutable layer, and both
the parent and the child restart over that frozen layer chain — so a
create-from-branch pays two Postgres startups (parent restart + child start)
instead of one, roughly 2× a plain branch create (~1.9 s p50 above). No data
is copied; the first (slowest) run replays the 1% UPDATE's WAL in both
instances, later runs freeze a near-empty rw layer. The ZFS backend
snapshots+clones the parent's dataset instead — no freeze or parent restart
at all. The CSI backend clones the parent's volume after a `CHECKPOINT` and
a brief stop of the parent, which is then restarted.

## Methodology

Everything below is what `hack/benchmark.sh` does; run it yourself with
`make build && hack/benchmark.sh` (it needs a local Docker engine: branch
ports are published on the engine host's `127.0.0.1`). The read-path
measurement in [Reads copy up too](#reads-copy-up-too) was taken by hand, with
the same `du -sb` helper.

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
perform where copy-up copies data (on a volume root where it clones, branchd
reports exclusive bytes instead; see
[usage accounting](concepts.md#usage-accounting)).

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
