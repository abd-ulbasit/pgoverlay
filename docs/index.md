# pgoverlay

`git branch` for Postgres: seed once from any running database, then spin up
isolated, writable copies that never write back to it.

Each branch is its own Postgres container whose data directory is an
**OverlayFS copy-on-write** mount over one shared, read-only seed of the
source. Creating a branch mounts that seed instead of copying it, so branches
start in about two seconds whatever the database size, run side by side, can
be reset, diffed against their base, and branched again.

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
    - **Reads copy data too.** Postgres opens table files read-write even to
      read them, and OverlayFS copies a file whole into the branch the first
      time that happens: branches grow toward the size of the tables they
      touch ([measurement](benchmarks.md#reads-copy-up-too)). For read-heavy
      branches of large databases, use the [zfs](zfs.md) or
      [csi](kubernetes.md) backend.
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
cloud, no fork of Postgres. If you *do* run ZFS, the
[experimental zfs backend](zfs.md) does block-level copy-on-write, and on
Kubernetes the [csi mode](kubernetes.md) clones volumes.

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
- [Benchmarks](benchmarks.md): measured numbers, and the OverlayFS copy-up
  diagnosis behind them.
- [Core concepts](concepts.md): copy-on-write and OverlayFS from first
  principles.
- [Architecture](architecture.md), [Code tour](code-tour.md),
  [Design decisions](DESIGN-DECISIONS.md) and [Deep dives](deep-dives.md):
  how it works, as built, and where the obvious implementation was wrong.
- [Upgrading to v1.0](upgrading.md): what changed since the release
  candidates.
