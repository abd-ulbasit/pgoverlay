# A real database for every test, in seconds

Mocked repositories and shared "test databases" both lie to you — the first
about SQL, the second about isolation. pgbranch gives each test its own
copy-on-write branch of a production-shaped database: full schema, full
(masked) data, isolated writes, destroyed when the test ends.

Everything below talks to a running [branchd](quickstart.md#run-the-server)
with at least one seeded source. The clients are deliberately thin: they
speak the same `/v1` REST API you can drive with `curl`.

## Go

```go
import (
    "database/sql"
    "testing"

    _ "github.com/jackc/pgx/v5/stdlib"

    "github.com/abd-ulbasit/pgbranch/pgbranchtest"
)

func TestOrderTotals(t *testing.T) {
    t.Parallel()
    b := pgbranchtest.Acquire(t) // creates the branch, waits until ready

    db, err := sql.Open("pgx", b.DSN)
    if err != nil {
        t.Fatal(err)
    }
    defer db.Close()
    // full production-shaped data; writes stay in this branch
}
```

`Acquire` reads its configuration from the environment and **skips the test**
when `PGBRANCH_SERVER` is unset, so the same suite runs plain unit tests on
laptops without a server and the real thing in CI:

| Variable | Meaning |
|---|---|
| `PGBRANCH_SERVER` | branchd base URL (unset ⇒ `t.Skip`) |
| `PGBRANCH_TOKEN` | API bearer token |
| `PGBRANCH_TEST_SOURCE` | default source name (else `main`) |
| `PGBRANCH_PASSWORD` | database password used in the returned DSNs (branch credentials are inherited from the source) |

Options: `pgbranchtest.WithSource("staging")` overrides the source,
`pgbranchtest.WithTTL(10*time.Minute)` the TTL. The returned `*Branch` has
`Name`, `Host`, `Port`, `User`, `Password`, `Database`, a direct `DSN`, and a
`ProxyDSN` that goes through the wire-protocol router (server host, port
6432, database `db@branch`).

The package is self-contained: importing it pulls in **no pgbranch
internals and no third-party dependencies**.

## JavaScript / TypeScript

`pgbranch-test` is a zero-dependency package (Node 18+, global `fetch`):

```js
import { acquire } from "pgbranch-test";

const b = await acquire(); // PGBRANCH_* env, same variables as Go
try {
  const client = new pg.Client({ connectionString: b.dsn });
  await client.connect();
  // ...
} finally {
  await b.destroy();
}
```

`acquire({server, token, source, ttlSeconds, name, password})` returns
`{branch, host, port, user, password, database, dsn, proxyDsn, destroy()}`.
With vitest/jest, call `acquire` in `beforeAll` and `destroy` in `afterAll`.
The package lives in [`sdk/js/`](https://github.com/abd-ulbasit/pgbranch/tree/main/sdk/js)
(not yet on npm — `npm pack` it or vendor the single `.mjs` file).

## GitHub Actions

```yaml
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: abd-ulbasit/pgbranch/action@main
        id: branch
        with:
          server: ${{ vars.PGBRANCH_SERVER }}
          token: ${{ secrets.PGBRANCH_TOKEN }}
          source: main
      - run: go test ./...
        env:
          DATABASE_URL: postgres://app:${{ secrets.DB_PASSWORD }}@${{ steps.branch.outputs.host }}:${{ steps.branch.outputs.port }}/${{ steps.branch.outputs.database }}
      - uses: abd-ulbasit/pgbranch/action/destroy@main
        if: always()
        with:
          server: ${{ vars.PGBRANCH_SERVER }}
          token: ${{ secrets.PGBRANCH_TOKEN }}
          branch: ${{ steps.branch.outputs.branch }}
```

The create action waits for the branch to report `ready` (polling up to
60×5s) before the next step runs. Outputs are `branch`, `host`, `port`,
`database` — **never the password**; the workflow already holds the
database credentials as its own secret. Inputs: `server`, `token`,
`source` (default `main`), `name` (default `t-gha-<run id>-<random>`),
`ttl` (seconds, default `3600`).

## Semantics

**Naming.** SDK-acquired branches are named
`t-<sanitized test name>-<random>` (Go uses `t.Name()`, left-truncated so the
most specific subtest part survives; JS generates `t-js-<random>`), capped at
the server's 41-char `^[a-z0-9][a-z0-9-]{0,40}$` rule. The `t-` prefix makes
test branches easy to spot — and easy to bulk-delete if a CI runner
disappears mid-run.

**Cleanup.** Explicit destroy is primary: Go registers it with `t.Cleanup`,
JS exposes `destroy()`, the Action has a companion destroy step. The TTL
(default **1 hour**) is the safety net for processes that die before cleanup
runs — the server-side reaper destroys expired branches automatically.

**Parallelism.** Every `Acquire` call gets its own branch with a random
suffix, so `t.Parallel()` tests, sharded CI jobs, and concurrent PR
pipelines never share state. Branch creation cost is the copy-on-write
clone — typically a few seconds regardless of database size (see
[benchmarks](benchmarks.md)).

**Credentials.** Branches inherit the source's credentials, so point
`PGBRANCH_PASSWORD` (or your workflow secret) at the source password. If a
future server rotates per-branch credentials, both SDKs already prefer the
server-returned password.
