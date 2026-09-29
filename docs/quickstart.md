# Quickstart

From nothing to branches on a laptop, first with the CLI alone, then with the
`branchd` server, its REST API, the Postgres router and the web UI.

## Requirements

- Docker: Docker Engine on Linux, or Docker Desktop / Colima on macOS. The
  Docker endpoint comes from `DOCKER_HOST` or your current docker context;
  `ssh://` contexts are not supported.
- `psql`, for the examples.
- The binaries: a [release archive](https://github.com/abd-ulbasit/pgoverlay/releases),
  `go install github.com/abd-ulbasit/pgoverlay/cmd/pgb@latest` (and
  `.../cmd/branchd@latest`; Go 1.26.6 or newer), or
  `make build` in a checkout (binaries in `./bin`).
- A source Postgres 14 to 18. `pg_basebackup` seeding needs
  `wal_level=replica` and a user with `REPLICATION`; managed Postgres uses
  [`--via dump`](#seeding-from-managed-postgres-supabase-neon-rds).

## A demo source

Skip this if you already have a Postgres that containers can reach.

```bash
docker run -d --name demo-src -e POSTGRES_PASSWORD=secret postgres:17 \
  -c wal_level=replica -c max_wal_senders=4
until docker exec demo-src pg_isready -h 127.0.0.1 -U postgres >/dev/null; do sleep 1; done
docker exec demo-src psql -U postgres \
  -c "CREATE TABLE t(i int); INSERT INTO t SELECT generate_series(1,100000);"

# The stock postgres image's pg_hba.conf has no remote *replication* entry
# (the catch-all "host all all all" doesn't match replication connections):
docker exec demo-src sh -c \
  'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
docker exec demo-src psql -U postgres -c "SELECT pg_reload_conf();"

# Per-network lookup: Docker 29 removed the top-level .NetworkSettings.IPAddress.
SRC_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' demo-src)
```

`pg_isready -h 127.0.0.1` waits for the real server: the image's
initialization server listens only on the Unix socket.

## Seed once, branch many

```bash
export PGPASSWORD=secret   # read by `source add`; branches accept the source's passwords
pgb source add main --host "$SRC_IP" --user postgres
# source "main" seeded and ready

pgb branch create pr-1 --from main
# branch "pr-1" ready in 2.482s (port 34467)

pgb branch ls
# NAME  PARENT  STATE  PORT   EXPIRES  CREATED
# pr-1  -       ready  34467  never    2026-09-28T19:46:50.881Z

psql "$(pgb connect pr-1)" -c "SELECT count(*) FROM t"   # 100000

# Writes stay in the branch — the source is mounted read-only underneath:
psql "$(pgb connect pr-1)" -c "DELETE FROM t WHERE i > 50000"
docker exec demo-src psql -U postgres -c "SELECT count(*) FROM t"  # still 100000

# Branch the branch: the child starts from pr-1's current state.
pgb branch create pr-1-child --from-branch pr-1
psql "$(pgb connect pr-1-child)" -c "SELECT count(*) FROM t"   # 50000

pgb branch destroy pr-1-child
pgb branch destroy pr-1
pgb source rm main
docker rm -f -v demo-src
```

Where the source must be reachable from: **containers**, since the seed runs
in a helper container.

- A database on the Docker host: `--host host.docker.internal` on Docker
  Desktop and Colima. Linux Docker Engine does not resolve that name inside
  containers; use the `docker0` gateway (usually `172.17.0.1`), the host's
  IP, or a user-defined network.
- A database in a container: its address (as above), or put it on a network
  and pass `--network <net>`.

The password comes from the variable named by `--password-env` (default
`PGPASSWORD`); `--no-password` is for trust, peer or certificate auth. State
lives in `~/.pgoverlay` (`PGOVERLAY_HOME`). Local-mode `pgb connect` prints the
branch URL without a password, because in the default inherit mode a branch
accepts the source's passwords: keep `PGPASSWORD` set, as above.

## The branch lifecycle

- **TTL**: `pgb branch create pr-1 --from main --ttl 24h`. Expired branches
  are destroyed by branchd's reconcile loop, or by `pgb gc` in local mode;
  local mode does not reap on its own (`pgb branch ls` notes expired ones).
- **Reset**: `pgb branch reset pr-1` discards the branch's writes and re-clones
  it from its base (a new container and port).
- **Recover**: `pgb branch recover pr-1` restarts a `failed` branch on its
  existing data, keeping its writes. See
  [Troubleshooting](troubleshooting.md#branch-states-and-how-to-get-out-of-them).
- **Refresh**: `pgb source refresh main` re-seeds the source into a new
  generation. Existing branches keep their snapshot; new branches see the
  fresh one.
- **Masking**: `pgb source set-mask main mask.sql` runs SQL inside every new
  or reset branch before it is ready (`get-mask`, `clear-mask`).
- **Diff**: `pgb diff pr-1` shows the schema diff and row-count changes
  against the branch's base.
- **Remove**: `pgb source rm main`, refused while the source has live branches.

## Seeding from managed Postgres (Supabase, Neon, RDS)

Managed providers don't allow physical replication connections, so
`pg_basebackup` can't seed from them. `--via dump` seeds with `pg_dump` piped
into a fresh cluster instead: it needs only a normal user (no `REPLICATION`
privilege), and can be scoped to schemas with repeatable `--dump-schema`:

```bash
PGOVERLAY_SEED_SSLMODE=require \
PGPASSWORD=... pgb source add prod --via dump --dump-schema public \
  --host db.<ref>.supabase.co --port 5432 --user postgres --pg-version 17
```

`--pg-version` must be **at least** the remote server's major (`pg_dump`
cannot dump newer servers); branches run on `--pg-version`. A logical dump is
slower than `pg_basebackup` at size, but branching afterwards is the same
either way. Over the REST API the same knobs are `"via": "dump"` and
`"dump_schemas": ["public"]` on `POST /v1/sources`.
`PGOVERLAY_SEED_SSLMODE` (default `prefer`) sets the seed connection's
`sslmode`; use `require` or `verify-full` over the internet. Roles,
extensions and row-level security policies have a few rules in dump mode:
see [Troubleshooting](troubleshooting.md#seeding).

## Supported Postgres versions

| Postgres major | 13 and older | 14 | 15 | 16 | 17 (default) | 18 |
|---|---|---|---|---|---|---|
| Supported | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |

Declare the source's major with `--pg-version` (default 17) so branches run a
matching `postgres:<major>` image; majors outside 14–18 are rejected at
registration, and a `basebackup` seed of a different major fails with the
version to use. A source that needs extensions or locales the stock image
lacks sets its own image with `--image` (for example
`postgis/postgis:17-3.5`). PG 13 and older lack
`recovery_init_sync_method=syncfs` (new in PG 14), which branch startup relies
on for fast crash recovery on the overlay.

## Run the server (`branchd`)

`branchd` is the daemon form: a REST API and a Postgres wire-protocol router
in one process, sharing the engine the CLI embeds, plus a reconcile loop that
reaps expired branches and repairs drift.

```bash
export PGOVERLAY_TOKEN=$(openssl rand -hex 16)   # at least 16 characters
branchd
# branch passwords are encrypted at rest under key a1940cb1 (from /home/you/.pgoverlay/secret.key)
# pg router listening on :6432 (connect with dbname@branch; TLS false)
# REST API listening on :7070 (TLS false)
# web UI at http://localhost:7070/ui/
```

Commonly used flags (all of them are in the [reference](reference.md#branchd)):

- `--api-addr :7070` and `--pg-addr :6432`: both bind every interface by
  default. Add `--api-tls-cert/--api-tls-key` and `--pg-tls-cert/--pg-tls-key`
  before exposing them.
- `--reconcile-interval 60s`: the reconcile tick (TTL reaping, drift repair,
  garbage collection). `--reap-interval` is a deprecated alias.
- `--stuck-timeout 10m`: how long an operation may go without progress before
  reconcile fails it, and the longest a branch operation through the API may
  run.
- `--rotate-branch-credentials`: every branch gets its own password,
  returned as `password` and encrypted at rest.
- `--default-ttl`, `--max-ttl`, `--max-branches`: limits on what clients
  create.
- `--cow overlay|zfs`: the copy-on-write backend (zfs is
  [experimental](zfs.md)).

Every `/v1` request needs `Authorization: Bearer <token>`. On
`SIGINT`/`SIGTERM` branchd finishes in-flight requests (up to
`--shutdown-timeout`, default 1m) and leaves branch containers running.

Reconcile is also exposed on demand: `pgb doctor` prints the drift plan
read-only (exit `1` if anything needs doing, `2` if the plan could not be
computed), and `pgb gc` applies it. Both work locally and against a server
(`GET /v1/reconcile/plan`, `POST /v1/reconcile`).

### REST API

The full list, with roles and error codes, is in [REST API](api.md).

```bash
AUTH="Authorization: Bearer $PGOVERLAY_TOKEN"

# sources (the password is used for the seed only; it is never written to the registry)
curl -H "$AUTH" -d '{"name":"main","host":"'"$SRC_IP"'","port":5432,
  "user":"postgres","pg_version":"17","password":"secret"}' localhost:7070/v1/sources
curl -H "$AUTH" localhost:7070/v1/sources
curl -H "$AUTH" -d '{"password":"secret"}' localhost:7070/v1/sources/main/refresh
curl -H "$AUTH" -X PUT -d '[{"name":"emails","sql":"UPDATE users SET email = md5(email)"}]' \
  localhost:7070/v1/sources/main/mask

# branches (ttl_seconds 0 or omitted: the server's --default-ttl, else never)
curl -H "$AUTH" -d '{"name":"pr-42","source":"main","ttl_seconds":86400}' localhost:7070/v1/branches
curl -H "$AUTH" -d '{"name":"pr-42-child","parent":"pr-42"}' localhost:7070/v1/branches
curl -H "$AUTH" localhost:7070/v1/branches
curl -H "$AUTH" localhost:7070/v1/branches/pr-42
curl -H "$AUTH" localhost:7070/v1/branches/pr-42/usage     # {"bytes":34742652}: rw-layer size
curl -H "$AUTH" localhost:7070/v1/branches/pr-42/history   # who changed it, when, why
curl -H "$AUTH" localhost:7070/v1/branches/pr-42/diff      # schema diff + row-count changes
curl -H "$AUTH" -X POST localhost:7070/v1/branches/pr-42/reset
curl -H "$AUTH" -X POST localhost:7070/v1/branches/pr-42/recover   # failed branches only
curl -H "$AUTH" -X DELETE localhost:7070/v1/branches/pr-42-child
curl -H "$AUTH" -X DELETE localhost:7070/v1/branches/pr-42

# scoped tokens (admin only); the token is printed once
curl -H "$AUTH" -d '{"name":"ci","role":"operator"}' localhost:7070/v1/tokens
curl -H "$AUTH" localhost:7070/v1/tokens
curl -H "$AUTH" -X DELETE localhost:7070/v1/tokens/ci

curl -H "$AUTH" -X DELETE localhost:7070/v1/sources/main
```

Request bodies are strict: an unknown field (`"ttl"` instead of
`"ttl_seconds"`) is a `400` naming it.

### One stable endpoint: the router

Instead of chasing per-branch host ports, connect to the router on `:6432`
with the branch name suffixed to the database:

```bash
psql "host=localhost port=6432 dbname=postgres@pr-42 user=postgres"
```

The router reads the startup message, resolves `pr-42` to its container,
rewrites the database back to `postgres`, and relays bytes transparently from
then on. Authentication (including SCRAM) happens between your client and the
branch's Postgres, untouched, and cancel requests (`Ctrl-C` in psql, driver
cancels) are routed to the right branch. An unknown or not-ready branch gets
`FATAL: pgoverlay: database not available`. A session that sends nothing in
either direction for 15 minutes is closed.

### CLI against the server

```bash
export PGOVERLAY_SERVER=http://localhost:7070   # or --server per command
export PGOVERLAY_TOKEN=<same token as branchd, or a scoped one>
pgb branch create pr-42 --from main --ttl 24h
pgb connect pr-42
# postgres://postgres@127.0.0.1:44879/postgres                # direct: the branchd host only
# postgres://postgres@localhost:6432/postgres@pr-42           # the router: use this elsewhere
pgb branch ls --usage    # adds a SIZE column (one helper container per branch)
```

The direct URL works only on the branchd host (Docker publishes branch ports
on `127.0.0.1`) or inside the cluster on Kubernetes. The router URL uses the
address branchd advertises (`--advertise-proxy-addr`), else the `--server`
host on port 6432; override it with `pgb connect --proxy-host/--proxy-port`.
For an `https://` server with a private CA, set `PGOVERLAY_CA_CERT=<pem file>`.

The registry is SQLite (single writer): don't run local-mode CLI commands
against the same `PGOVERLAY_HOME` while branchd is running; use server mode.

### Web UI

branchd serves a small embedded web UI at `http://localhost:7070/ui/`: a
static page baked into the binary, no build toolchain, no CDN. Paste a token
once per browser tab (it is kept in the tab's `sessionStorage`); the page
lists sources and branches with state, endpoint, expiry countdown and disk
usage, has create, reset (with a confirmation) and destroy controls, and
refreshes every 5 seconds. Recovering a failed branch and adding sources are
CLI and API only.
