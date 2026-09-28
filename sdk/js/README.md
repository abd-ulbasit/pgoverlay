# pgoverlay-test

A disposable copy-on-write Postgres branch for every test, backed by a
running [pgoverlay](https://github.com/abd-ulbasit/pgoverlay) server. Zero
dependencies; Node 18+ (global `fetch`).

```js
import pg from "pg";
import { acquire } from "pgoverlay-test";

const b = await acquire(); // PGOVERLAY_SERVER / PGOVERLAY_TOKEN from env
try {
  const client = new pg.Client({ connectionString: b.proxyDsn });
  await client.connect();
  // full production-shaped data, writes stay in the branch
} finally {
  await b.destroy();
}
```

Options (all optional): `{ server, token, source, ttlSeconds, name, password,
proxyHost, pollIntervalMs, timeoutMs }`. Defaults come from
`PGOVERLAY_SERVER`, `PGOVERLAY_TOKEN`, `PGOVERLAY_TEST_SOURCE` (else `main`),
`PGOVERLAY_PROXY_HOST` and `PGOVERLAY_PASSWORD`. See `index.d.ts` for the full
shapes and the
[testing guide](https://github.com/abd-ulbasit/pgoverlay/blob/main/docs/testing.md)
for semantics (naming, parallelism, cleanup).

- **Connect through `proxyDsn`.** It goes through the pgoverlay router
  (`proxyHost`, else the server's host, port 6432). `dsn` targets the
  branch's own Postgres, which only the branchd host (Docker) or pods inside
  the cluster (Kubernetes) can reach. On the Helm chart the router is its own
  Service: set `proxyHost` (or `PGOVERLAY_PROXY_HOST`) to it, for example
  `pgoverlay-proxy.pgoverlay-system:6432`.
- **Cleanup.** `destroy()` is the primary cleanup. The TTL (`ttlSeconds`,
  default 3600) is a server-side safety net for runs that die first;
  `ttlSeconds: 0` turns it off (the server's `--default-ttl` applies, if
  any, otherwise the branch is never reaped). If the branch fails, is
  destroyed while `acquire` waits, or is not ready within `timeoutMs`,
  `acquire` destroys it and rejects.
- **Credentials.** A per-branch password returned by the server (branchd
  `--rotate-branch-credentials`) wins; otherwise branches inherit the
  source's credentials and `password` / `PGOVERLAY_PASSWORD` is used.

## Develop

```sh
node --test test/*.test.mjs   # unit tests against an in-process stub server
npm pack --dry-run            # verify the publishable file list
```

Or from the repo root: `make js-sdk-test`. The package ships `index.mjs`,
`index.d.ts`, this README and the Apache-2.0 `LICENSE`.

## Publish (manual, not wired to CI)

```sh
cd sdk/js
npm version <patch|minor>
npm publish --access public
```
