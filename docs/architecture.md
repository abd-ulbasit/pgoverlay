# Architecture (as built)

How pgbranch actually works after Phase 4 — written from the code rather than
from the original design spec, so where the two disagree this document follows
the code.

## Components

```
            pgb (CLI) ────────────────┐
            │ local mode:             │ server mode: REST + bearer token
            │ embeds the engine       ▼
            │                ┌─ branchd ────────────────────────────────┐
            │                │  REST API :7070   (+ embedded web UI)    │
            │                │  pgproxy :6432    (wire-protocol router) │
            │                │  TTL reaper       (background loop)      │
            │                └──────────────┬────────────────────────────┘
            ▼                               ▼
        ┌─ engine ──────────────────────────────────┐
        │  sagas: create / reset / destroy / seed   │
        │  cow.Planner: overlay | zfs (layer names, │
        │  entrypoints, zfs argv)                   │
        └───────┬──────────────────────┬────────────┘
                ▼                      ▼
        registry (SQLite)      runtime.Driver
        states + journal       ├─ DockerDriver (containers, volumes)
        sources, branches,     └─ KubeDriver   (pods on one storage node,
        mask scripts                            hostPath "volumes")
```

One engine, two frontends: the CLI embeds it directly (local mode), branchd
serves it over REST and the CLI becomes a thin API client (server mode). The
registry is SQLite — single writer, which is why branchd is single-replica
and local mode must not run concurrently with it.

## The CoW mechanism (overlay backend, default)

`pgb source add` runs `pg_basebackup` in a one-shot helper container,
streaming the source cluster into a named volume — that volume is the
read-only **lower layer** for every branch. `pgb branch create` makes one
empty volume for the branch's writes and starts a stock postgres container
whose entrypoint assembles an OverlayFS mount *inside the container*:

```
 ┌─ branch container (CAP_SYS_ADMIN) ──────────────────────────┐
 │   PGDATA = /pgbranch/merged   ← overlayfs mount             │
 │                ▲                                            │
 │     ┌──────────┴───────────┐                                │
 │     │ upper+work (writes)  │  volume: pgbranch-br-pr-1-rw   │
 │     ├──────────────────────┤                                │
 │     │ lower (read-only)    │  volume: pgbranch-src-main ────┼─▶ shared by
 │     └──────────────────────┘  (pg_basebackup snapshot)      │   all branches
 │   entrypoint.sh: mount overlay → exec docker-entrypoint.sh  │
 └─────────────────────────────────────────────────────────────┘
```

Mounting in-container (not on the host) is what makes the same code work on
Colima/macOS and bare Linux. Postgres boots on the merged view and performs
ordinary WAL crash recovery, as if the machine power-cycled at backup time.

One non-obvious flag is load-bearing: the entrypoint execs postgres with
`-c recovery_init_sync_method=syncfs`. The default (`fsync`) opens every data
file read-write before recovery, and a read-write open of a lower-layer file
forces a full OverlayFS copy-up — i.e. a complete copy of the database into
the branch's rw layer. `syncfs` replaces that per-file pass with one syscall
and copies nothing up; it's what makes branch creation O(1) in data size
(measured in [Benchmarks](benchmarks.md)).

## The zfs backend (experimental)

`branchd --cow zfs --zfs-dataset tank/pgbranch` swaps the layer mechanics:
sources seed into datasets (`<prefix>/src-<name>-gN`), branch create is
`zfs snapshot` + `zfs clone` (block-level CoW — no copy-up problem, no
overlay assembly; the entrypoint shrinks to perms + pid cleanup + exec), and
zfs commands run in privileged helpers with `/dev/zfs` mapped in. Same
engine, same sagas — `cow.Planner` decides what the driver is asked to do.
Details and verification walkthrough: [ZFS backend](zfs.md).

## Sagas and states

Branch rows move through a journaled state machine:

```
 creating ──► ready ──► resetting ──► ready
     │          │           │
     ▼          ▼           ▼
   failed   destroying ──► destroyed        (every transition journaled)
```

Provisioning is a **saga**: each step (layer create, entrypoint install,
container start, readiness wait, masking) registers a compensation, and any
failure unwinds them in reverse — no orphaned containers, volumes, or
datasets. Masking scripts (per-source ordered SQL, stored in the registry)
run inside the fresh branch via `psql` over the local socket *after*
readiness and *before* the branch is marked ready, so a branch never serves
unmasked data; a failing script fails the branch. On startup, `Reconcile`
repairs interrupted work: stuck `creating` rows are failed and their
resources cleaned, and managed containers with no registry row are removed.

## Source generations

`pgb source refresh` seeds a **new** layer (`...-g2`, `-g3`, …) and bumps the
source's generation. Existing branches keep the layer they were cloned from;
new branches use the new one. An old generation is garbage-collected when the
last branch referencing it is destroyed. Branches never follow the source —
a branch is a point-in-time snapshot by design.

## Proxy routing

Per-branch host ports are annoying, so branchd bundles a wire-protocol
router. Clients connect to `:6432` with the branch name suffixed to the
database name:

```
 psql "dbname=postgres@pr-42" ──► pgproxy reads the startup message,
                                  resolves pr-42 → container host:port
                                  (registry lookup), rewrites dbname back
                                  to "postgres", then splices bytes
```

Everything after the startup message — including SCRAM authentication — is
relayed untouched between client and branch.

## Kubernetes: the storage-node model

The kube driver maps the same abstractions onto one designated **storage
node**: "volumes" are subdirectories of a data root (default
`/var/lib/pgbranch`) mounted via `hostPath`; helpers are one-shot pods and
branches are plain pods, all pinned to that node with `nodeName`. No CSI, no
CRDs, no operator — branchd is a normal Deployment with a namespace-scoped
Role. This is deliberately the honest dev/test scope; multi-node storage is
future work.

## What runs where

| concern | mechanism |
|---|---|
| data files | only ever touched **inside containers** (helpers/entrypoints) |
| host Go code | pure control plane: registry, sagas, driver API calls |
| seeding | `pg_basebackup` helper, runs as uid 999 (postgres) |
| disk usage | `du -sb` helper on the rw layer (zfs: `zfs list -o used`) |
| web UI | single static page, `go:embed`, no build toolchain |
| GitHub App | separate `pgbranch-github` service driving the REST API |
