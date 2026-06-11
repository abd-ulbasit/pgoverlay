# Quickstart

> Adapted from the [README](https://github.com/abd-ulbasit/pgbranch); the
> README stays the canonical copy of this walkthrough.

Requirements: Docker (Colima works on macOS), Go 1.26+ to build. The source
database needs `wal_level=replica` and a user with `REPLICATION` privilege
(`pg_basebackup` does the seeding) — or use `--via dump` for managed
Postgres, see below.

```bash
make build   # produces ./bin/pgb (CLI) and ./bin/branchd (daemon)
```

## Demo source

Skip if you already have a Postgres reachable from containers:

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

## Seed once, branch many

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

`--host` must be reachable *from containers* (use `host.docker.internal` for
a host-local DB, or `--network <net>` for a DB on a Docker network). The
password is read from the env var named by `--password-env` (default
`PGPASSWORD`). State lives in `~/.pgbranch` (override with `PGBRANCH_HOME`).

## Seeding from managed Postgres (Supabase, Neon, RDS)

Managed providers don't allow physical replication connections, so
`pg_basebackup` can't seed from them. `--via dump` seeds with `pg_dump` piped
into a fresh cluster instead — it needs only a normal user (no `REPLICATION`
privilege), and can be scoped to schemas with repeatable `--dump-schema`:

```bash
PGPASSWORD=... ./bin/pgb source add prod --via dump --dump-schema public \
  --host db.<ref>.supabase.co --port 5432 --user postgres --pg-version 17
```

`--pg-version` must be **>=** the remote server's major version (`pg_dump`
cannot dump newer servers); branches run on `--pg-version`. A logical dump is
slower than `pg_basebackup` at size, but branching afterwards is the same
instant CoW either way. Over the REST API the same knobs are
`"via": "dump"` and `"dump_schemas": ["public"]` on `POST /v1/sources`.

## Supported Postgres versions

| Postgres major | 13 and older | 14 | 15 | 16 | 17 (default) | 18 |
|---|---|---|---|---|---|---|
| Supported | ❌ | ✅ | ✅ | ✅ | ✅ | ✅ |

Declare the source's major with `--pg-version` (default 17) so branches run a
matching `postgres:<major>` image; majors outside 14–18 are rejected at
registration. PG 13 and older lack `recovery_init_sync_method=syncfs` (new in
PG 14), which branch startup relies on for fast crash recovery on the overlay.

Branches can self-destruct (`--ttl 24h`, reaped by `branchd`), be reset to
their source snapshot (`pgb branch reset pr-1` — discards all writes, new
container/port), and sources can be re-seeded (`pgb source refresh main` —
existing branches keep their old snapshot; new branches see the fresh one) or
removed (`pgb source rm main`).

## Run the server (`branchd`)

`branchd` is the daemon form: a REST API and a Postgres wire-protocol router
in one process, sharing the engine the CLI embeds, plus a TTL reaper for
abandoned branches.

```bash
PGBRANCH_TOKEN=$(openssl rand -hex 16) ./bin/branchd
# REST API listening on :7070
# pg router listening on :6432 (connect with dbname@branch)
```

Flags: `--api-addr :7070` (REST), `--pg-addr :6432` (router),
`--reap-interval 30s` (TTL reaper tick), `--cow overlay|zfs` (CoW backend —
zfs is [experimental](zfs.md)). `PGBRANCH_TOKEN` is required; every `/v1`
request needs `Authorization: Bearer <token>` (`GET /healthz` is open).

### REST API

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
curl -H "$AUTH" localhost:7070/v1/branches/pr-42/usage   # {"bytes":N} — rw-layer size
curl -H "$AUTH" -X POST localhost:7070/v1/branches/pr-42/reset
curl -H "$AUTH" -X DELETE localhost:7070/v1/branches/pr-42
```

### One stable endpoint: the router

Instead of chasing per-branch host ports, connect to the router on `:6432`
with the branch name suffixed to the database:

```bash
psql "host=localhost port=6432 dbname=postgres@pr-42 user=postgres"
```

The router reads the startup message, resolves `pr-42` to its container,
rewrites the database back to `postgres`, and relays bytes transparently from
then on — authentication (including SCRAM) happens between your client and
the branch's Postgres, untouched.

### CLI against the server

```bash
export PGBRANCH_SERVER=http://localhost:7070   # or --server per command
export PGBRANCH_TOKEN=<same token as branchd>
pgb branch create pr-42 --from main --ttl 24h
pgb connect pr-42        # direct-port URL and the :6432 proxy URL
pgb branch ls --usage    # adds a SIZE column (one helper container per branch)
```

The registry is SQLite (single-writer): don't run local-mode CLI commands
against the same `PGBRANCH_HOME` while branchd is running — use server mode.

### Web UI

branchd serves a small embedded web UI at `http://localhost:7070/ui/` — a
single static page baked into the binary. Paste your `PGBRANCH_TOKEN` once
(kept in localStorage); the page lists sources and branches with state,
endpoint, expiry countdown and disk usage, with create/reset/destroy
controls. Auto-refreshes every 5 seconds.
