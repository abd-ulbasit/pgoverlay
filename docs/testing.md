# A real database for every test, in seconds

Mocked repositories and shared "test databases" both lie to you — the first
about SQL, the second about isolation. pgoverlay gives each test its own
copy-on-write branch of a production-shaped database: full schema, full
(masked) data, isolated writes, destroyed when the test ends.

Everything below talks to a running [branchd](quickstart.md#run-the-server-branchd)
with at least one seeded source. The clients are deliberately thin: they
speak the same `/v1` REST API you can drive with `curl`.

## Where to connect: the router, not the branch

Every client hands you two ways to reach a branch. **Use the router one.**

- **Through the router** (`ProxyDSN`, `proxyDsn`, the Action's `proxy_*`
  outputs): branchd's Postgres wire-protocol router, port 6432, database
  `db@branch`. One stable endpoint that works from anywhere the router is
  reachable: a CI runner, a laptop, a pod.
- **Directly** (`DSN`, `dsn`, the Action's `host`/`port`): the branch's own
  Postgres. The Docker runtime publishes it on `127.0.0.1` of the branchd
  host, and the Kubernetes runtime reports a pod IP, so it only works from
  the branchd host itself or from inside the cluster. A hosted runner such as
  `ubuntu-latest` can reach neither.

The clients assume the router listens on the same host as the REST API (a
single branchd). When it has its own address, such as the Helm chart's
separate `pgoverlay-proxy` Service, point them at it: `PGOVERLAY_PROXY_HOST`
/ `WithProxyHost` in Go, `proxyHost` in JS, the `proxy_host` input of the
Action — for example `pgoverlay-proxy.pgoverlay-system:6432` in-cluster, or
the NodePort/LoadBalancer address from outside.

## Go

```go
import (
    "database/sql"
    "testing"

    _ "github.com/jackc/pgx/v5/stdlib"

    "github.com/abd-ulbasit/pgoverlay/pgoverlaytest"
)

func TestOrderTotals(t *testing.T) {
    t.Parallel()
    b := pgoverlaytest.Acquire(t) // creates the branch, waits until ready

    db, err := sql.Open("pgx", b.ProxyDSN)
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()
    // full production-shaped data; writes stay in this branch
}
```

`Acquire` reads its configuration from the environment and **skips the test**
when `PGOVERLAY_SERVER` is unset, so the same suite runs plain unit tests on
laptops without a server and the real thing in CI:

| Variable | Meaning |
|---|---|
| `PGOVERLAY_SERVER` | branchd base URL (unset ⇒ `t.Skip`) |
| `PGOVERLAY_TOKEN` | API bearer token |
| `PGOVERLAY_TEST_SOURCE` | default source name (else `main`) |
| `PGOVERLAY_PROXY_HOST` | `host[:port]` of the router for `ProxyDSN` (else the `PGOVERLAY_SERVER` host, port 6432) |
| `PGOVERLAY_PASSWORD` | database password used in the returned DSNs when the server does not return a per-branch one (see [Credentials](#semantics)) |

Options: `pgoverlaytest.WithSource("staging")` overrides the source,
`pgoverlaytest.WithTTL(10*time.Minute)` the TTL (see
[Cleanup](#semantics); `WithTTL(0)` turns the safety net off), and
`pgoverlaytest.WithProxyHost("pgoverlay-proxy.pgoverlay-system:6432")` the
router address. The returned `*Branch` has `Name`, `Host`, `Port`, `User`,
`Password`, `Database`, a `ProxyDSN` through the router, and a direct `DSN`.

The package is self-contained: importing it pulls in **no pgoverlay
internals and no third-party dependencies**.

## JavaScript / TypeScript

`pgoverlay-test` is a zero-dependency package (Node 18+, global `fetch`):

```js
import pg from "pg";
import { acquire } from "pgoverlay-test";

const b = await acquire(); // PGOVERLAY_* env, same variables as Go
try {
  const client = new pg.Client({ connectionString: b.proxyDsn });
  await client.connect();
  // ...
} finally {
  await b.destroy();
}
```

`acquire({server, token, source, ttlSeconds, name, password, proxyHost})`
returns `{branch, host, port, user, password, database, dsn, proxyDsn,
destroy()}`. If the branch fails, is destroyed while `acquire` waits, or is
not ready in time, `acquire` destroys it and rejects. With vitest/jest, call
`acquire` in `beforeAll` and `destroy` in `afterAll`. The package lives in
[`sdk/js/`](https://github.com/abd-ulbasit/pgoverlay/tree/main/sdk/js)
(not yet on npm — `npm pack` it or vendor the single `.mjs` file).

## GitHub Actions

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: abd-ulbasit/pgoverlay/action@v1
        id: branch
        with:
          server: ${{ vars.PGOVERLAY_SERVER }}
          token: ${{ secrets.PGOVERLAY_TOKEN }}
          source: main
      - run: go test ./...
        env:
          PGHOST: ${{ steps.branch.outputs.proxy_host }}
          PGPORT: ${{ steps.branch.outputs.proxy_port }}
          PGDATABASE: ${{ steps.branch.outputs.proxy_database }}
          PGUSER: ${{ steps.branch.outputs.user }}
          PGPASSWORD: ${{ steps.branch.outputs.password || secrets.DB_PASSWORD }}
      - uses: abd-ulbasit/pgoverlay/action/destroy@v1
        if: always()
        with:
          server: ${{ vars.PGOVERLAY_SERVER }}
          token: ${{ secrets.PGOVERLAY_TOKEN }}
          branch: ${{ steps.branch.outputs.branch }}
```

The libpq `PG*` variables are read by `psql`, pgx, node-postgres and most
other drivers, and they need no escaping. If your code wants a single
`DATABASE_URL` instead, build `postgres://user:password@host:port/db@branch`
from the same outputs and percent-encode the password.

**Pinning.** `@v1` is a floating tag: it was published with
`v1.0.0-rc.4` and moves to the newest compatible release, the Actions
convention (see
[SECURITY.md](https://github.com/abd-ulbasit/pgoverlay/blob/main/SECURITY.md)).
Never use `@main`, which runs whatever was pushed last. For an immutable
reference, pin a full release tag (for example `action@v1.0.0-rc.4`) or,
stricter still, a commit SHA, since tags can be moved.

The action is a composite action that only talks to the `/v1` REST API — it
ships no binary and is not tied to the `branchd` version you run.

The create action waits for the branch to report `ready` (polling up to
60×5s, and failing at once if the branch turns `failed` or is destroyed).
Inputs: `server`, `token`, `source` (default `main`), `name` (default
`t-gha-<run id>-<random>`), `ttl` (seconds, default `3600`; `0` turns the
safety net off), `proxy_host` (the router's `host[:port]`, default the
server's host on port 6432). Outputs:

| Output | Meaning |
|---|---|
| `branch` | branch name; set as soon as the branch exists, even if it then fails to become ready, so the destroy step can remove it |
| `proxy_host`, `proxy_port`, `proxy_database` | where to connect: the router, database `db@branch` |
| `user` | database user |
| `password` | only when branchd runs `--rotate-branch-credentials`, masked in the log; empty otherwise |
| `host`, `port`, `database` | the direct address — only from the branchd host or inside the cluster |

The destroy action treats an empty `branch` (the create step failed before
creating a branch) as a no-op with a warning, rejects anything that is not a
branch name (pass the `branch` output, not a git ref), and counts a 404 as
success only when branchd itself reports the branch gone.

## Semantics

**Naming.** SDK-acquired branches are named
`t-<sanitized test name>-<random>` (Go uses `t.Name()`, left-truncated so the
most specific subtest part survives; JS generates `t-js-<random>`), capped at
the server's 41-char `^[a-z0-9][a-z0-9-]{0,40}$` rule. The `t-` prefix makes
test branches easy to spot — and easy to bulk-delete if a CI runner
disappears mid-run.

**Cleanup.** Explicit destroy is primary: Go registers it with `t.Cleanup`
(before waiting for the branch, so a branch that never becomes ready is
removed too), JS exposes `destroy()` and destroys a branch `acquire` gives up
on, the Action has a companion destroy step. The TTL (default **1 hour**) is
the safety net for processes that die before cleanup runs — the server-side
reaper destroys expired branches automatically. A TTL of `0` turns the
safety net off: the server applies its `--default-ttl` if one is set,
otherwise such a branch is never reaped. The server may also shorten a TTL
to its `--max-ttl`. Go sends the TTL in whole seconds, rounded up.

**Parallelism.** Every `Acquire` call gets its own branch with a random
suffix, so `t.Parallel()` tests, sharded CI jobs, and concurrent PR
pipelines never share state. Branch creation cost is the copy-on-write
clone — typically a few seconds regardless of database size (see
[benchmarks](benchmarks.md)).

**Credentials.** By default branches inherit the source's credentials:
point `PGOVERLAY_PASSWORD` (or your workflow secret) at the source password.
When branchd runs with `--rotate-branch-credentials`, every branch gets its
own generated password instead, returned by the API. Both SDKs prefer that
server-returned password, and the Action exposes it as its masked `password`
output, which the workflow above falls back from. Applications that keep
static configuration can fetch it at startup with
[`pgoverlayconnect`](https://github.com/abd-ulbasit/pgoverlay/tree/main/pgoverlayconnect).
