# pgoverlay

`git branch` for Postgres: seed once from any running database, then spin up
isolated, writable copies that never write back to it.

Each branch is its own Postgres container whose data directory is an
**OverlayFS copy-on-write** mount over one shared, read-only seed of the
source. Creating a branch mounts that seed instead of copying it, so branches
start in about two seconds whatever the database size, run side by side, can
be reset, diffed against their base, and branched again. Reads in a branch
copy nothing; a write copies the file it touches once, or only the blocks it
changes where the volumes sit on XFS or btrfs.

```
$ pgb branch create pr-1 --from main
branch "pr-1" ready in 2.482s (port 34467)
```

**Measured:** a 1 GiB and a 5 GiB database both branch in ~1.9 s (p50 of 5
runs), and a fresh branch holds 33.1 MiB of its own data, not a copy of the
dataset. Full results and methodology in [Benchmarks](benchmarks.md).

!!! warning "Honest limits"
    - **A dev/test tool.** Branches are disposable Postgres instances for
      development, CI, review apps and migration rehearsal: no backups, no
      replication of branches, no merge-back, and a branch never follows its
      source after seeding.
    - **Branch containers are privileged.** On Docker and in Kubernetes
      hostpath mode every branch container gets `CAP_SYS_ADMIN` with AppArmor
      unconfined, for its overlay mount. Kubernetes csi mode adds no
      capabilities. See [Security](security.md).
    - **On ext4, a write copies the whole file.** Reads copy nothing, but the
      first write to a table or index file copies that file (a segment, up to
      1 GiB) into the branch, and that write waits for the copy. Where the
      volumes live on XFS (`reflink=1`) or btrfs, the copy is an extent clone
      and a write copies only the blocks it changes; branchd detects this, and
      `--volume-root` puts the volumes on such a disk
      ([how](concepts.md#8-clone-or-copy-what-a-copy-up-costs)).
    - **Linux 4.19 or later for copy-free reads.** Each branch checks at start
      that its kernel and image can run the lazyrw shim. Where they cannot,
      it copies every table file it opens, as releases before v1.0.0 did, and
      says so in its log and in `pgoverlay_branch_cow_mode`
      ([Troubleshooting](troubleshooting.md#a-branch-copies-eagerly)).
    - **Postgres 14 to 18**, Linux containers; one `branchd` writes the
      registry (more replicas are failover, not scale-out).

## The problem

Every team wants production-like databases for development, CI, and PR review
apps. A `pg_dump`/`pg_restore` or `createdb -T` is a full copy every time:
minutes to hours for real datasets, and N copies cost N times the disk.
Copy-on-write branching fixes both, and there are several ways to get it:
hosted platforms (Neon, Supabase branching), self-hosted systems built on ZFS
or LVM (DBLab Engine) or on Kubernetes storage (Xata), and PostgreSQL 18's
in-instance database cloning on reflink filesystems. The README
[compares them](https://github.com/abd-ulbasit/pgoverlay#how-it-compares).

pgoverlay takes the middle path: plain Docker, stock Postgres images, and
OverlayFS copy-on-write (the mechanism container images use) applied to
`PGDATA`, against the Postgres you already run. No special filesystem, no
cloud, no fork of Postgres: a small preload library keeps Postgres's reads
from copying files, and a filesystem that can clone (XFS or btrfs) turns the
file copies of writes into block-level copy-on-write. If you *do* run ZFS,
the [experimental zfs backend](zfs.md) does block-level copy-on-write on its
own, and on Kubernetes the [csi mode](kubernetes.md) clones volumes.

## Where to go

- [Quickstart](quickstart.md): Docker on a laptop, the CLI, and the `branchd`
  server.
- [Ways to use it](usage.md): local dev, a database per test, a branch per
  PR, preview environments, reviewing migrations.
- [Reference](reference.md) and [REST API](api.md): every command, flag,
  variable and endpoint.
- [Troubleshooting](troubleshooting.md): failed branches, recovery,
  reachability.
- [Security](security.md): threat model and hardening checklist.
- [Kubernetes](kubernetes.md): branch pods on a storage node or as CSI
  clones, and the Helm chart.
- [GitHub App](github-app.md): a database branch per pull request.
- [Benchmarks](benchmarks.md): measured numbers, the OverlayFS copy-up
  diagnosis behind them, and what copy-on-write costs since v1.0.0.
- [Core concepts](concepts.md): copy-on-write and OverlayFS from first
  principles.
- [Architecture](architecture.md), [Code tour](code-tour.md),
  [Design decisions](DESIGN-DECISIONS.md) and [Deep dives](deep-dives.md):
  how it works, as built, and where the obvious implementation was wrong.
- [Upgrading to v1.0](upgrading.md): what changed since the release
  candidates, and the
  [changelog](https://github.com/abd-ulbasit/pgoverlay/blob/main/CHANGELOG.md).
