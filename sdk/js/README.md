# pgoverlay-test

A disposable copy-on-write Postgres branch for every test, backed by a
running [pgoverlay](https://github.com/abd-ulbasit/pgoverlay) server. Zero
dependencies; Node 18+ (global `fetch`).

```js
import { acquire } from "pgoverlay-test";

const b = await acquire(); // PGOVERLAY_SERVER / PGOVERLAY_TOKEN from env
try {
  const client = new pg.Client({ connectionString: b.dsn });
  await client.connect();
  // full production-shaped data, writes stay in the branch
} finally {
  await b.destroy();
}
```

Options (all optional): `{ server, token, source, ttlSeconds, name, password,
pollIntervalMs, timeoutMs }`. Defaults come from `PGOVERLAY_SERVER`,
`PGOVERLAY_TOKEN`, `PGOVERLAY_TEST_SOURCE` (else `main`) and
`PGOVERLAY_PASSWORD`; the TTL defaults to 1 hour as a safety net — `destroy()`
is the primary cleanup. See `index.d.ts` for the full shapes and the
[testing guide](https://github.com/abd-ulbasit/pgoverlay/blob/main/docs/testing.md)
for semantics (naming, parallelism, cleanup).

## Develop

```sh
node --test test/*.test.mjs   # unit tests against an in-process stub server
npm pack --dry-run       # verify the publishable file list
```

Or from the repo root: `make js-sdk-test`.

## Publish (manual, not wired to CI)

```sh
cd sdk/js
npm version <patch|minor>
npm publish --access public
```
