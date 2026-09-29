# Changelog

Notable changes to pgoverlay. Each release's GitHub page also lists every
commit since the previous tag. Upgrading from a release candidate:
[docs/upgrading.md](docs/upgrading.md).

## v1.0.0 (unreleased)

### True copy-on-write ([#49](https://github.com/abd-ulbasit/pgoverlay/issues/49))

A read in a branch no longer copies a table into it. Before this release,
PostgreSQL's read-write opens made OverlayFS copy every table file a branch
touched, reads included: one `SELECT count(*)` of a 489 MB table grew a
branch from 33 MiB to 523 MiB. In the evaluation, a read of a 521 MB frozen
table added 555 MiB and took 20.8 s without the fix, and 0 bytes and 277 ms
with it ([benchmarks](docs/benchmarks.md#true-copy-on-write-v100)).

Added:

- **The lazyrw shim, on by default on the overlay backend** (Docker and
  Kubernetes hostpath). An `LD_PRELOAD` library in the branch's Postgres
  opens relation files read-only and reopens a file read-write on its first
  write, so reads copy nothing and a write copies the file it touches once.
  `TRUNCATE`, `DROP` and table rewrites copy nothing. Each branch self-tests
  its kernel (Linux 4.19 or later) and probes the build for its image's libc
  and architecture at start; where either fails it copies on open as before,
  with a warning. PG 18 branches pin `io_method=worker` while it is active.
  Builds for glibc and musl on x86_64 and aarch64 are committed with
  `SHA256SUMS`, embedded in the binaries, and rebuilt byte for byte in CI.
  ([#50](https://github.com/abd-ulbasit/pgoverlay/pull/50),
  [#53](https://github.com/abd-ulbasit/pgoverlay/pull/53))
- **Seed settle**: every new seed is recovered, `VACUUM (FREEZE, ANALYZE)`d
  (with `pg_statistic` frozen last) and cleanly shut down once, on
  pgoverlay's copy, never the source. Branches start without WAL replay, and
  their reads set no hint bits. The settle then trims the seed's last WAL
  segment to a hole after its checkpoint, byte for byte the same file, so a
  branch's first WAL write copies about 1 MiB instead of 16 MiB (measured on
  ext4: 12-27 ms instead of 46-119 ms), and removes unused segments after it.
  ([#52](https://github.com/abd-ulbasit/pgoverlay/pull/52),
  [#56](https://github.com/abd-ulbasit/pgoverlay/pull/56))
- **Alpine and custom images as sources**: seed helpers run as the image's
  own `postgres` user (999:999 on Debian, 70:70 on Alpine) instead of uid
  999, so `postgres:*-alpine` sources seed, and their branches run the musl
  build of the shim. ([#56](https://github.com/abd-ulbasit/pgoverlay/pull/56))
- **Block-level copy-on-write where the filesystem clones.** branchd probes
  at startup whether OverlayFS copy-up copies data or clones extents (XFS
  with `reflink=1`, btrfs), counts branch usage as exclusive bytes in clone
  mode with an embedded static tool (`pgoverlay-du`), and on XFS sets a
  16 KiB copy-on-write extent size hint on a volume root it manages.
  ([#51](https://github.com/abd-ulbasit/pgoverlay/pull/51))
- **`--volume-root DIR`** (docker runtime, `PGOVERLAY_VOLUME_ROOT`): create
  every volume as a bind volume under `DIR` on the Docker host, for example
  on an XFS or btrfs disk, without moving Docker.
- **Settings**: branchd `--lazyrw=on|off` (`PGOVERLAY_LAZYRW`, Helm
  `cow.lazyrw`), `--seed-settle=freeze|recover|off`
  (`PGOVERLAY_SEED_SETTLE`, Helm `seedSettle`), `--volume-root`,
  `--xfs-cowextsize`, and the experimental `--wal-recycle=off`
  (`PGOVERLAY_WAL_RECYCLE`). `pgb` in local mode reads the same environment
  variables.
- **Metrics**: `pgoverlay_branch_cow_mode{mode}` (ready overlay branches by
  `lazyrw`, `eager`, `off`, `unknown`) and `pgoverlay_cow_copyup_mode{mode}`
  (`clone`, `copy`, `unknown`).
- **CI**: `lazyrw` (the committed builds are reproducible; C tests on a real
  overlay), `pg-import-audit` (every write-class libc import of `postgres`
  in `postgres:14` to `18` and `17-alpine` is interposed or reviewed) and
  `integration-arm64`.
- **Tests and benchmarks**: a copy-on-write torture suite (reads, writes,
  `TRUNCATE`/`DROP`, `VACUUM FULL`, `CREATE INDEX`, `CREATE DATABASE`,
  `SIGKILL` mid-TPC-B with `pg_amcheck`, branch-from-branch, reset, diff),
  the version matrix with the shim on 14 to 18 and `17-alpine`, and
  `hack/bench-cow.sh`, an interleaved pgbench comparison of plain Postgres,
  eager and lazyrw branches, and a reflink volume root.
  ([#57](https://github.com/abd-ulbasit/pgoverlay/pull/57),
  [#58](https://github.com/abd-ulbasit/pgoverlay/pull/58))

Changed:

- On ext4 the first write to a table or index segment copies the segment
  (up to 1 GiB) into the branch and waits for it. The write that waits is
  usually a checkpoint writing the page out, not the statement that changed
  it. On XFS and btrfs volume roots the copy is a clone, and later writes
  copy only the blocks they change.
- Seeding takes longer by the settle: roughly one read of the database plus
  a write of its unfrozen pages. A source whose configuration cannot start
  in a container (an `include` of a file outside the data directory) now
  fails at seed time instead of at branch start.
- `pgb diff` compares seeded tables by planner estimates, because settled
  seeds are analyzed: a small change shows once the branch has analyzed the
  table. `--seed-settle=recover` keeps the old exact counting.

Upgrade notes:

- Branches created by an earlier build keep copying eagerly (reported as
  `eager`) until they are reset, recovered, or branched from. Seeds taken
  before this release are not settled; `pgb source refresh` takes a settled
  generation for new branches.
- No registry migration and no new privileges. branchd runs one extra
  helper at startup (the copy-up probe) with a branch container's
  privileges.
- On hosts where branch volumes sit on XFS with `reflink=1`, run a kernel
  with the fix for CVE-2026-64600
  ([docs/security.md](docs/security.md#xfs-reflink-hosts-cve-2026-64600)).

Known gaps: only linux/amd64 on one kernel was measured by hand; arm64 is
covered by CI; Docker Desktop, Colima and OrbStack are expected to work but
were not measured. The pgbench release gate ran on a shared, loaded host:
warm select-only throughput is within noise of plain Postgres and the eager
branch, but warm TPC-B (9.4% below the eager branch on medians, with a
spread several times larger) is inconclusive there and needs a quiet-host
rerun ([benchmarks](docs/benchmarks.md#throughput-and-the-first-write-stall)).
`--wal-recycle=off` was measured and saves no copy-up on a settled seed; it
stays an experimental opt-in. A managed loopback XFS pool for block-level
copy-on-write on ext4 hosts is planned for v1.1 as an opt-in.

### Also since v1.0.0-rc.4

Hardening and fixes from the pre-v1 review; the behaviour changes are listed
in [docs/upgrading.md](docs/upgrading.md#behaviour-changes). Highlights:

- Tag-triggered releases with goreleaser: binaries for Linux and macOS
  (amd64, arm64) with checksums and build provenance, and multi-arch images
  (linux/amd64, linux/arm64) with an SBOM.
- A dedicated at-rest key for rotated branch passwords (`secret.key`),
  independent of `PGOVERLAY_TOKEN`; an undecryptable password is reported as
  `password_unavailable` instead of failing the branch.
- `pgb branch recover` and `POST /v1/branches/{name}/recover`; reconcile
  repairs ready branches whose container is gone, stopped or moved, and
  heartbeats keep slow operations from being failed as stuck.
- Standby sources, per-source branch images (`--image`), major-version
  checks at seed time, and branches that never inherit the source's ports,
  sockets, archiving or replication settings.
- Stricter REST API decoding and clearer statuses (`422`, `503`, `504`, and
  a failed destroy's cause); the router advertises its address in branch
  responses.
- GitHub App branch names keyed by repository, ordered deliveries, and
  public statuses that never carry internal errors.
- Helm: leader-routed API Service, helper Secrets, least-privilege ghook,
  Pod Security guidance, and `networkPolicy.sourceEgress` on seed helpers.
- A fail-closed `govulncheck` gate with a per-advisory allowlist.

Earlier release candidates (`v1.0.0-rc.1` to `v1.0.0-rc.4`) are described on
their [GitHub release pages](https://github.com/abd-ulbasit/pgoverlay/releases).
