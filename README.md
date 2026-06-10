# pgbranch

`git branch` for Postgres: seed once from any running database, then spin up isolated, writable copies in ~2.5 seconds — without copying the data.

```
$ pgb branch create pr-1 --from main
branch "pr-1" ready in 2.533s (port 32774)
```

Measured on a Colima VM (macOS Virtualization.Framework, Apple Silicon) against a freshly seeded source. Branch creation time is dominated by Postgres crash recovery, not data size — a 100 GB source branches in roughly the same time as a 100 MB one.

## The problem

Every team wants production-like databases for development, CI, and PR review apps. The options today:

- **`pg_dump`/`pg_restore` or `createdb -T`** — a full physical copy every time. Minutes to hours for real datasets, and N copies cost N times the disk.
- **Neon / Supabase branching** — genuinely instant, but cloud-only. Your data lives on their storage layer; you can't point them at the Postgres you already run.
- **DBLab (Database Lab Engine)** — self-hosted thin clones, but built around ZFS (or LVM) pools you must provision and operate.

pgbranch takes the middle path: plain Docker, plain Postgres images, and OverlayFS copy-on-write — the same mechanism container images use — applied to `PGDATA`. No special filesystem, no cloud, no fork of Postgres.

## Quickstart

Requirements: Docker (Colima works on macOS), Go 1.26+ to build. The source database needs `wal_level=replica` and a user with `REPLICATION` privilege (pg_basebackup does the seeding).

```bash
make build   # produces ./bin/pgb (CLI) and ./bin/branchd (daemon)
```

Demo source (skip if you already have a Postgres reachable from containers):

```bash
docker run -d --name demo-src -e POSTGRES_PASSWORD=secret postgres:17 \
  -c wal_level=replica -c max_wal_senders=4
docker exec demo-src sh -c 'until pg_isready -U postgres; do sleep 1; done'
docker exec demo-src psql -U postgres \
  -c "CREATE TABLE t(i int); INSERT INTO t SELECT generate_series(1,100000);"

# The stock postgres image's pg_hba.conf has no remote *replication* entry
# (the catch-all "host all all all" doesn't match replication connections):
docker exec demo-src sh -c \
  'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
docker exec demo-src psql -U postgres -c "SELECT pg_reload_conf();"

SRC_IP=$(docker inspect -f '{{.NetworkSettings.IPAddress}}' demo-src)
```

Seed once, branch many:

```bash
PGPASSWORD=secret ./bin/pgb source add main --host "$SRC_IP" --user postgres

./bin/pgb branch create pr-1 --from main
# branch "pr-1" ready in 2.533s (port 32774)

./bin/pgb branch ls
psql "$(./bin/pgb connect pr-1)" -c "SELECT count(*) FROM t"   # 100000

# Writes stay in the branch — the source is mounted read-only underneath:
psql "$(./bin/pgb connect pr-1)" -c "DELETE FROM t WHERE i > 50000"
docker exec demo-src psql -U postgres -c "SELECT count(*) FROM t"  # still 100000

./bin/pgb branch destroy pr-1
docker rm -f demo-src
```

`--host` must be reachable *from containers* (use `host.docker.internal` for a host-local DB, or `--network <net>` for a DB on a Docker network). The password is read from the env var named by `--password-env` (default `PGPASSWORD`). State lives in `~/.pgbranch` (override with `PGBRANCH_HOME`).

Branches can self-destruct (`--ttl 24h`, reaped by `branchd`), be reset to their source snapshot (`pgb branch reset pr-1` — discards all writes, new container/port), and sources can be re-seeded (`pgb source refresh main` — existing branches keep their old snapshot; new branches see the fresh one) or removed (`pgb source rm main`).

## Run the server (`branchd`)

`branchd` is the daemon form: a REST API and a Postgres wire-protocol router in one process, sharing the engine the CLI embeds, plus a TTL reaper for abandoned branches.

```bash
make build                       # produces ./bin/pgb and ./bin/branchd
PGBRANCH_TOKEN=$(openssl rand -hex 16) ./bin/branchd
# 2026/06/10 12:00:00 REST API listening on :7070
# 2026/06/10 12:00:00 pg router listening on :6432 (connect with dbname@branch)
```

Flags: `--api-addr :7070` (REST), `--pg-addr :6432` (router), `--reap-interval 30s` (TTL reaper tick). `PGBRANCH_TOKEN` is required — branchd refuses to start without it; every `/v1` request needs `Authorization: Bearer <token>` (`GET /healthz` is open). `SIGINT`/`SIGTERM` shut down gracefully and leave branch containers running.

REST API:

```bash
AUTH="Authorization: Bearer $PGBRANCH_TOKEN"

# sources (the password is used for pg_basebackup only — never stored)
curl -H "$AUTH" -d '{"name":"main","host":"host.docker.internal","port":5432,
  "user":"postgres","pg_version":"17","password":"secret"}' localhost:7070/v1/sources
curl -H "$AUTH" localhost:7070/v1/sources
curl -H "$AUTH" -d '{"password":"secret"}' localhost:7070/v1/sources/main/refresh
curl -H "$AUTH" -X DELETE localhost:7070/v1/sources/main

# branches (ttl_seconds=0 or omitted = never reaped)
curl -H "$AUTH" -d '{"name":"pr-42","source":"main","ttl_seconds":86400}' localhost:7070/v1/branches
curl -H "$AUTH" localhost:7070/v1/branches
curl -H "$AUTH" localhost:7070/v1/branches/pr-42
curl -H "$AUTH" -X POST localhost:7070/v1/branches/pr-42/reset
curl -H "$AUTH" -X DELETE localhost:7070/v1/branches/pr-42
```

**One stable endpoint for every branch.** Instead of chasing per-branch host ports, connect to the router on `:6432` with the branch name suffixed to the database:

```bash
psql "host=localhost port=6432 dbname=postgres@pr-42 user=postgres"
```

The router reads the startup message, resolves `pr-42` to its container, rewrites the database back to `postgres`, and relays bytes transparently from then on — authentication (including SCRAM) happens between your client and the branch's Postgres, untouched.

The CLI drives a running branchd in server mode:

```bash
export PGBRANCH_SERVER=http://localhost:7070   # or --server per command
export PGBRANCH_TOKEN=<same token as branchd>
pgb branch create pr-42 --from main --ttl 24h
pgb connect pr-42    # prints the direct-port URL and the :6432 proxy URL
```

Honest caveat: the registry is SQLite, which is single-writer. Don't run local-mode CLI commands (no `--server`) against the same `PGBRANCH_HOME` while branchd is running — use server mode; that's the supported combination.

## How it works

`pgb source add` runs `pg_basebackup` in a one-shot helper container, streaming the source cluster into a named Docker volume. That volume becomes the read-only **lower layer** for every branch.

`pgb branch create` creates one empty volume for the branch's writes, then starts a stock `postgres:17` container with a tiny entrypoint that assembles an OverlayFS mount *inside the container* (so the same code works on Colima/macOS and bare Linux — volumes sit on ext4 inside the VM):

```
            host (pgb CLI)
            │  SQLite registry · saga orchestration · Docker API
            ▼
 ┌─ branch container (CAP_SYS_ADMIN) ──────────────────────────┐
 │                                                             │
 │   PGDATA = /pgbranch/merged   ← overlayfs mount             │
 │                ▲                                            │
 │     ┌──────────┴───────────┐                                │
 │     │ upper+work (writes)  │  volume: pgbranch-br-pr-1-rw   │
 │     ├──────────────────────┤                                │
 │     │ lower (read-only)    │  volume: pgbranch-src-main ────┼─▶ shared by
 │     └──────────────────────┘  (pg_basebackup snapshot)      │   all branches
 │                                                             │
 │   entrypoint.sh: mount overlay → exec docker-entrypoint.sh  │
 └─────────────────────────────────────────────────────────────┘
```

Postgres starts on the merged view and performs ordinary WAL crash recovery — exactly as if the machine had power-cycled at backup time. Pages a branch modifies are copied up into its own volume on first write; everything else is read through the shared lower layer. Branches are fully isolated from the source and from each other.

Host-side Go code is pure control plane: a SQLite registry with a journaled state machine, and create/destroy implemented as sagas (every step registers a compensation, so a failure mid-create leaves no orphan containers or volumes).

## Scope: what this is and isn't

pgbranch is a **dev/test tool**. Branches are disposable Postgres instances for development, CI, PR review apps, and migration rehearsal.

It is **not** a production database platform: no HA, no replication of branches, no backups, no connection pooling, and the branch container needs `CAP_SYS_ADMIN` (for the overlay mount) — fine for a dev box or CI runner, not something to expose to untrusted workloads. A branch is a point-in-time snapshot; it does not follow the source after seeding.

Phase 1 also branches only from sources, not from other branches (layer-DAG branching is planned).

## Comparison

|  | pgbranch | Neon | DBLab (DLE) | pg_dump/restore |
|---|---|---|---|---|
| Branch creation | seconds, CoW | seconds, CoW | seconds, CoW | minutes–hours, full copy |
| Disk per branch | only changed pages | only changed pages | only changed pages | full copy |
| Works with your existing Postgres | yes (pg_basebackup from any PG) | no — data must live in Neon | yes | yes |
| Self-hosted | yes | cloud service | yes | yes |
| Infra requirements | Docker only | — | ZFS/LVM pool to provision | none |
| Postgres | stock images | forked storage engine | stock | stock |
| Production-grade HA | no (dev/test tool) | yes | no (dev/test tool) | n/a |

## Roadmap

- **Phase 2** ✅ — `pgproxy` wire-protocol router (one stable endpoint, route by branch name), REST API + auth (`branchd` daemon reusing the same engine), TTL reaper for abandoned branches, branch reset, source refresh with generations. Branch-from-branch moved to a later phase.
- **Phase 3** — Kubernetes runtime driver, Helm chart, GitHub App (a branch per PR, automatically), branch-from-branch.
- **Phase 4** — data masking hooks, web UI, ZFS backend as an alternative CoW engine, published benchmarks, docs site.

## Development

```bash
make test   # unit tests
make it     # integration tests (needs Docker): PGBRANCH_IT=1, ~min on first pull
make lint   # go vet
```

## License

[Apache-2.0](LICENSE) — Copyright 2026 Abdul Basit.
