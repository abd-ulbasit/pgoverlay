# pgbranch

[![ci](https://github.com/abd-ulbasit/pgbranch/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/abd-ulbasit/pgbranch/actions/workflows/ci.yml)

`git branch` for Postgres: seed once from any running database, then spin up isolated, writable copies without ever touching the source.

![pgbranch demo](docs/demo.gif)

*branching a 1 GiB database, recorded for real — see [docs/benchmarks.md](docs/benchmarks.md)*

**Measured:** pgbranch branches a 1 GiB database in ~1.9 s and a 5 GiB database in ~1.9 s (p50 of 5 runs, Colima VM on Apple Silicon) — creation time is independent of database size, and a fresh branch costs ~33 MiB of disk, not a copy of the dataset. Full results, methodology, and the diagnosis of the copy-up bug this fixed are in [docs/benchmarks.md](docs/benchmarks.md).

## The problem

Every team wants production-like databases for development, CI, and PR review apps. The options today:

- **`pg_dump`/`pg_restore` or `createdb -T`** — a full physical copy every time. Minutes to hours for real datasets, and N copies cost N times the disk.
- **Neon / Supabase branching** — genuinely instant, but cloud-only. Your data lives on their storage layer; you can't point them at the Postgres you already run.
- **DBLab (Database Lab Engine)** — self-hosted thin clones, but built around ZFS (or LVM) pools you must provision and operate.

pgbranch takes the middle path: plain Docker, plain Postgres images, and OverlayFS copy-on-write — the same mechanism container images use — applied to `PGDATA`. No special filesystem, no cloud, no fork of Postgres.

## Quickstart

Requirements: Docker (Colima works on macOS), Go 1.26+ to build. The source database needs `wal_level=replica` and a user with `REPLICATION` privilege (pg_basebackup does the seeding) — or use `--via dump` for managed Postgres, see below.

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

### Seeding from managed Postgres (Supabase, Neon, RDS)

Managed providers don't allow physical replication connections, so `pg_basebackup` can't seed from them. `--via dump` seeds with `pg_dump` piped into a fresh cluster instead — it needs only a normal user (no `REPLICATION` privilege), and can be scoped to schemas:

```bash
PGPASSWORD=... ./bin/pgb source add prod --via dump --dump-schema public \
  --host db.<ref>.supabase.co --port 5432 --user postgres --pg-version 17
```

`--pg-version` must be **>=** the remote server's major version (`pg_dump` cannot dump newer servers); branches run on `--pg-version`. A logical dump is slower than `pg_basebackup` at size, but branching afterwards is the same instant CoW either way.

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
curl -H "$AUTH" localhost:7070/v1/branches/pr-42/usage   # {"bytes":N} — rw-layer size (runs a helper container)
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

`pgb branch ls --usage` adds a SIZE column showing each branch's copy-on-write rw layer (its own writes, not the shared source data). It runs one helper container per branch, so it's opt-in.

Honest caveat: the registry is SQLite, which is single-writer. Don't run local-mode CLI commands (no `--server`) against the same `PGBRANCH_HOME` while branchd is running — use server mode; that's the supported combination.

## Web UI

branchd serves a small embedded web UI at `http://localhost:7070/ui/` (the exact URL is logged at startup) — a single static page baked into the binary, no build toolchain, no CDN, works air-gapped. Paste your `PGBRANCH_TOKEN` once (kept in the browser's localStorage); the page lists sources and branches with state, endpoint, expiry countdown and rw-layer disk usage, and has create/reset/destroy controls. Auto-refreshes every 5 seconds.

*(screenshot placeholder: dark monospace dashboard with sources and branches tables)*

## Run on Kubernetes

branchd can run in-cluster with branches as pods (`--runtime kube`). A Helm chart deploys the whole thing — for a soup-to-nuts AWS walkthrough (Terraform, images, LoadBalancers, version upgrades, and the production bugs found doing it) see [docs/eks.md](docs/eks.md):

```bash
make docker-build                          # builds pgbranch/branchd:dev (push it, or `kind load` for local clusters)
helm install pgbranch deploy/helm/pgbranch \
  --namespace pgbranch-system --create-namespace \
  --set node=<storage-node-name> \
  --set token=$(openssl rand -hex 16)
```

Values that matter:

- **`node` (required)** — the name of the **storage node** (`kubectl get nodes`). All CoW data lives under `dataRoot` (default `/var/lib/pgbranch`) on this one node as plain directories; branchd, every branch pod, and every helper pod are pinned there with `nodeName` + `hostPath`. This is the default `hostpath` storage mode; set `storage.mode=csi` with `storage.storageClass=<class supporting PVC cloning>` for multi-node storage — branches become PVC clones, pods schedule on any node, and no `SYS_ADMIN` is needed (see [docs/kubernetes.md](docs/kubernetes.md)).
- **`token` / `existingSecret`** — the REST API bearer token. Either let the chart render a Secret from `token`, or point `existingSecret` at a pre-created Secret with key `token`.
- **`proxy.service.type`** — set to `NodePort` (with `proxy.service.nodePort`) to reach branches from outside the cluster without a port-forward.

The chart creates a single-replica Deployment (branchd's registry is SQLite — single writer, so one replica, `Recreate` strategy, state in `hostPath <dataRoot>/state` on the storage node), a namespace-scoped Role (pods create/delete/get/list/watch, pods/exec, pods/log — branchd manages pods only in its own namespace), and two Services: `pgbranch-api` (REST, :7070) and `pgbranch-proxy` (Postgres router, :6432). The branchd container runs as root for write access to its hostPath state dir; branch pods get `CAP_SYS_ADMIN` for their in-container overlay mount, same as on Docker.

Using it is the same REST API as above; branch hosts are pod IPs, so connect via the proxy Service:

```bash
kubectl -n pgbranch-system port-forward svc/pgbranch-api 7070 &
curl -H "$AUTH" -d '{"name":"main","host":"db.prod.internal","port":5432,
  "user":"postgres","password":"secret"}' localhost:7070/v1/sources
curl -H "$AUTH" -d '{"name":"pr-42","source":"main"}' localhost:7070/v1/branches

# in-cluster: psql "host=pgbranch-proxy.pgbranch-system port=6432 dbname=postgres@pr-42 user=postgres"
kubectl -n pgbranch-system port-forward svc/pgbranch-proxy 6432 &
psql "host=localhost port=6432 dbname=postgres@pr-42 user=postgres"
```

`make helm-test` lints and grep-asserts the rendered chart; `make k8s-it` runs the full integration suite against a local [kind](https://kind.sigs.k8s.io) cluster (`hack/kind-up.sh` creates `pgbranch-test` and preloads images).

## Branch per pull request

`pgbranch-github` (`cmd/pgbranch-github`, image `pgbranch/ghook` via `make docker-build-ghook`) turns pull requests into branches: a signed GitHub webhook creates `pr-<number>` when a PR opens, optionally resets it on every push, destroys it on close, and can post a one-time connect-info comment on the PR. The Helm chart ships it as an optional sub-deployment (`--set ghook.enabled=true ...`). Setup, permissions, and the full `GHOOK_*` environment reference live in [docs/github-app.md](docs/github-app.md).

See it end-to-end on a real pull request — a migration that passes on an empty dev database, fails against the PR's masked clone of production (37 legacy duplicate emails), gets fixed, and the branch is destroyed on merge: [pgbranch-demo PR #1](https://github.com/abd-ulbasit/pgbranch-demo/pull/1).

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

For hosts that already run ZFS there is an **experimental zfs backend** (`branchd --cow zfs --zfs-dataset tank/pgbranch`): branches become `zfs snapshot` + `zfs clone` instead of overlay layers — block-level CoW, no whole-file copy-up. It is unit-tested with manual-verification instructions (no ZFS in this project's CI); see [docs/zfs.md](docs/zfs.md) before relying on it.

## Scope: what this is and isn't

pgbranch is a **dev/test tool**. Branches are disposable Postgres instances for development, CI, PR review apps, and migration rehearsal.

It is **not** a production database platform: no HA, no replication of branches, no backups, no connection pooling, and the branch container needs `CAP_SYS_ADMIN` (for the overlay mount) — fine for a dev box or CI runner, not something to expose to untrusted workloads. A branch is a point-in-time snapshot; it does not follow the source after seeding.

pgbranch also branches only from sources, not from other branches (layer-DAG branching is future work).

## Supported Postgres versions

| Postgres major | 13 and older | 14 | 15 | 16 | 17 (default) | 18 |
|---|---|---|---|---|---|---|
| Supported | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |

Declare the major when registering a source (`pgb source add main --pg-version 16 …`, or `"pg_version":"16"` over the API); branches then run `postgres:<major>` so the binary matches the seeded data directory. Versions outside 14–18 are rejected at registration time.

PG 13 and older are unsupported because branch startup passes `-c recovery_init_sync_method=syncfs`, a GUC added in **PG 14** — it is what makes WAL crash recovery on a fresh overlay fast (one `syncfs()` instead of fsyncing every data file; see [benchmarks](docs/benchmarks.md)). The matrix is exercised end-to-end by `make matrix` (seed → branch → verify → destroy per major; defaults to 14 and 18, the range edges).

## Comparison

|  | pgbranch | Neon | DBLab (DLE) | pg_dump/restore |
|---|---|---|---|---|
| Branch creation | seconds, CoW | seconds, CoW | seconds, CoW | minutes–hours, full copy |
| Disk per branch | rw overlay, ~33 MiB + files written ([benchmarks](docs/benchmarks.md)) | only changed pages | only changed pages | full copy |
| Works with your existing Postgres | yes (pg_basebackup from any PG) | no — data must live in Neon | yes | yes |
| Self-hosted | yes | cloud service | yes | yes |
| Infra requirements | Docker only | — | ZFS/LVM pool to provision | none |
| Postgres | stock images | forked storage engine | stock | stock |
| Production-grade HA | no (dev/test tool) | yes | no (dev/test tool) | n/a |

## Roadmap

- **Phase 2** ✅ — `pgproxy` wire-protocol router (one stable endpoint, route by branch name), REST API + auth (`branchd` daemon reusing the same engine), TTL reaper for abandoned branches, branch reset, source refresh with generations. Branch-from-branch moved to a later phase.
- **Phase 3** ✅ — Kubernetes runtime driver (branch pods on a storage node), Helm chart, GitHub webhook service (a branch per PR, automatically).
- **Phase 4** ✅ — data masking hooks, embedded web UI with per-branch disk usage, published benchmarks (with the copy-up fix they motivated), experimental ZFS backend, docs site.
- **Phase 5** ✅ — TLS (router + REST API), Postgres 14–18 support matrix, branch-from-branch (frozen-layer DAG), multi-node CSI storage for Kubernetes (PVC-clone branches, no `SYS_ADMIN`, any node).
- **Future** — merge-back of branch data and multi-writer branches remain non-goals; ideas welcome in issues.

## Documentation

`docs/` is a small MkDocs site — no hosting or CI, build it locally with `pip install mkdocs-material && mkdocs serve`:

- [Quickstart](docs/quickstart.md) — Docker on a laptop: CLI, `branchd`, REST API, router, web UI.
- [Kubernetes](docs/kubernetes.md) — Helm chart, the storage-node model.
- [GitHub App](docs/github-app.md) — a database branch per pull request.
- [Benchmarks](docs/benchmarks.md) — measured numbers, methodology, and the copy-up diagnosis.
- [ZFS backend](docs/zfs.md) — experimental; requirements and manual verification walkthrough.
- [Architecture](docs/architecture.md) — components, CoW mechanics, sagas, generations, routing — as built.

## Development

```bash
make test    # unit tests
make it      # integration tests (needs Docker): PGBRANCH_IT=1, ~min on first pull
make matrix  # Postgres version matrix (PGBRANCH_MATRIX_VERSIONS="14 18" by default)
make lint    # go vet
```

## License

[Apache-2.0](LICENSE) — Copyright 2026 Abdul Basit.
