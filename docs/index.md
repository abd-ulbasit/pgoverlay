# pgbranch

`git branch` for Postgres: seed once from any running database, then spin up
isolated, writable copies without ever touching the source.

```
$ pgb branch create pr-1 --from main
branch "pr-1" ready in 2.533s (port 32774)
```

**Measured:** pgbranch branches a 1 GiB database in ~1.9 s and a 5 GiB
database in ~1.9 s (p50 of 5 runs) — creation time is independent of database
size, and a fresh branch costs ~33 MiB of disk, not a copy of the dataset.
Full results and methodology in [Benchmarks](benchmarks.md).

## The problem

Every team wants production-like databases for development, CI, and PR review
apps. The options today:

- **`pg_dump`/`pg_restore` or `createdb -T`** — a full physical copy every
  time. Minutes to hours for real datasets, N copies cost N× the disk.
- **Neon / Supabase branching** — genuinely instant, but cloud-only; you
  can't point them at the Postgres you already run.
- **DBLab (Database Lab Engine)** — self-hosted thin clones, but built around
  ZFS (or LVM) pools you must provision and operate.

pgbranch takes the middle path: plain Docker, plain Postgres images, and
OverlayFS copy-on-write — the same mechanism container images use — applied
to `PGDATA`. No special filesystem, no cloud, no fork of Postgres. (If you
*do* run ZFS, an [experimental zfs backend](zfs.md) does block-level CoW.)

## Where to go

- [Quickstart](quickstart.md) — Docker on a laptop, CLI and `branchd` server.
- [Kubernetes](kubernetes.md) — branch pods on a storage node, Helm chart.
- [GitHub App](github-app.md) — a database branch per pull request.
- [Benchmarks](benchmarks.md) — real measured numbers and the copy-up
  diagnosis behind them.
- [Architecture](architecture.md) — how it actually works, as built.

## Scope

pgbranch is a **dev/test tool**: disposable Postgres instances for
development, CI, PR review apps, and migration rehearsal. It is not a
production database platform — no HA, no backups, and branch containers need
`CAP_SYS_ADMIN` for their overlay mount. A branch is a point-in-time
snapshot; it does not follow the source after seeding.
