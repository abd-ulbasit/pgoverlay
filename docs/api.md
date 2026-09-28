# REST API (`/v1`)

`branchd` serves a JSON API on `--api-addr` (default `:7070`). The CLI in
server mode, the GitHub webhook service, the SDKs and the Action all use it,
and you can drive it with `curl`.

```bash
AUTH="Authorization: Bearer $PGOVERLAY_TOKEN"
curl -H "$AUTH" localhost:7070/v1/branches
```

## Authentication and roles

Every `/v1` request needs `Authorization: Bearer <token>`. The token is either
`PGOVERLAY_TOKEN` (the built-in admin, at least 16 characters) or a stored
token minted with `pgb token create NAME --role ROLE` / `POST /v1/tokens`.
Roles are ranked `viewer < operator < admin`; a route admits its minimum role
and everything above it. A missing or unknown token gets `401`, a role below
the route's minimum gets `403`.

`/healthz` (liveness), `/readyz` (registry reachable and runtime responding),
`/metrics` (Prometheus) and the static web UI under `/ui/` need no token and
are not part of the versioned contract.

## Endpoints

"Leader" marks routes that only the elected leader serves when branchd runs
with leader election ([High availability](ha.md)); on a follower they return
`503 not leader` after the token and role checks. They also run on a context
detached from the client connection, so a dropped connection does not abort a
half-done operation, and branch operations are bounded by `--stuck-timeout`.

| Method and path | Min role | Leader | Request body | Success |
|---|---|---|---|---|
| `POST /v1/sources` | admin | yes | `CreateSourceRequest` | `201` `Source` |
| `GET /v1/sources` | viewer | | | `200` `[Source]` |
| `DELETE /v1/sources/{name}` | admin | yes | | `204` |
| `POST /v1/sources/{name}/refresh` | admin | yes | `{"password": "..."}` | `200` `Source` |
| `PUT /v1/sources/{name}/mask` | admin | yes | `[MaskScript]` (`[]` clears) | `200` `[MaskScript]` |
| `GET /v1/sources/{name}/mask` | viewer | | | `200` `[MaskScript]` |
| `POST /v1/branches` | operator | yes | `CreateBranchRequest` | `201` `Branch` |
| `GET /v1/branches` | viewer | | | `200` `[Branch]` (live branches) |
| `GET /v1/branches/{name}` | viewer | | | `200` `Branch` |
| `GET /v1/branches/{name}/usage` | viewer | | | `200` `{"bytes": N}` |
| `GET /v1/branches/{name}/history` | viewer | | | `200` `[Transition]` |
| `GET /v1/branches/{name}/diff[?data=N]` | operator | yes | | `200` `DiffResult` |
| `POST /v1/branches/{name}/reset` | operator | yes | | `200` `Branch` |
| `POST /v1/branches/{name}/recover` | operator | yes | | `200` `Branch` |
| `DELETE /v1/branches/{name}` | operator | yes | | `204` |
| `GET /v1/reconcile/plan` | viewer | | | `200` `ReconcilePlan` |
| `POST /v1/reconcile` | operator | yes | | `200` `ReconcilePlan` (actions taken) |
| `POST /v1/tokens` | admin | yes | `{"name": "...", "role": "..."}` | `201` `{"token": "..."}` |
| `GET /v1/tokens` | admin | | | `200` `[Token]` |
| `DELETE /v1/tokens/{name}` | admin | yes | | `204` |

Notes:

- **Source create and refresh are long requests**: they return once the seed
  (`pg_basebackup` or `pg_dump`) has finished. The password in the body is
  used for that seed only; it is never stored or returned.
- **Branch create, reset and recover** return once the branch is ready
  (seconds for a plain branch; longer for a large first recovery or masking).
- **Reset** discards the branch's writes and re-clones it from its base. It
  also works on a `failed` branch, which retries a failed create.
- **Recover** restarts a `failed` branch on its existing data, with no
  re-clone, masking or credential rotation: the way back for a branch that
  failed with its volumes intact (for example a branch-from-branch parent
  interrupted by a crash). `409` when the branch is not failed or its volumes
  are gone.
- **Destroy** of a branch stuck in `destroying` (an earlier destroy failed
  part-way) retries the teardown.
- **Diff** is a `GET` but provisions a throwaway Postgres instance and writes
  a registry row, so it needs the operator role and the leader. `?data=N`
  (0 to 500) samples up to N new rows per grown table. On the csi backend,
  diffing a branch that was created from another branch briefly stops and
  restarts that parent.
- **Usage** measures the branch's own layer with a one-shot helper, so it
  costs a container start. On csi it reports the clone's full size as the
  filesystem sees it, not the copy-on-write delta.
- **History** keeps working after the branch is destroyed and after its source
  is removed, and records failed destroy attempts as `destroying -> destroying`
  entries.

## Objects

Fields marked "optional" are omitted from responses when empty. Clients must
ignore fields they do not recognise.

### `CreateSourceRequest`

| Field | Default | Meaning |
|---|---|---|
| `name` | required | source name: `^[a-z0-9][a-z0-9-]{0,40}$` |
| `host` | required | source host as reachable **from containers**; blank is rejected |
| `port` | `5432` | |
| `user` | `postgres` | seed user (`REPLICATION` for `basebackup`) |
| `database` | `postgres` | database recorded for connection strings, and dumped with `via=dump` |
| `network` | | Docker network to run the seed helper on |
| `pg_version` | `17` image | source major, `14` to `18`; branches run `postgres:<pg_version>` |
| `password` | | seed password (empty for trust, peer or certificate auth) |
| `via` | `basebackup` | `basebackup` or `dump` |
| `dump_schemas` | whole database | schemas to dump (`via=dump` only; no commas in a pattern) |
| `image` | `postgres:<pg_version>` | image for the seed helpers and every branch, for extensions, locales or libc, e.g. `postgis/postgis:17-3.5` |

### `Source`

`name`, `pg_version`, `host`, `port`, `user`, `database`, `network`
(optional), `via`, `dump_schemas` (optional), `state` (`seeding`, `ready` or
`failed`), `generation` (bumped by each refresh), `created_at`, `image`
(optional; absent when the default image is used).

### `CreateBranchRequest`

| Field | Meaning |
|---|---|
| `name` | branch name: `^[a-z0-9][a-z0-9-]{0,40}$` |
| `source` | source to branch from; exactly one of `source` and `parent` |
| `parent` | branch to branch from (branch-from-branch) |
| `ttl_seconds` | expire after this many seconds. `0` or omitted means the server's `--default-ttl` if one is set, otherwise never; every TTL is capped at `--max-ttl` |

### `Branch`

| Field | Meaning |
|---|---|
| `name`, `source` | |
| `parent` | optional; the branch it was created from |
| `state` | `creating`, `ready`, `resetting`, `failed`, `destroying` or `destroyed` |
| `host`, `port` | the branch's own Postgres: `127.0.0.1` on the Docker host, a pod IP on Kubernetes |
| `user`, `database` | the source's connection role and database |
| `password` | optional; the branch's own password when branchd runs `--rotate-branch-credentials` |
| `password_unavailable` | optional; `true` when a rotated password is stored but cannot be decrypted with the configured at-rest key. `password` is then omitted; reset the branch to mint a new one |
| `proxy_database` | the database name to use through the router: `<database>@<branch>` |
| `proxy_host`, `proxy_port` | optional; the router address branchd advertises (`--advertise-proxy-addr`; the port defaults to `--pg-addr`'s). Without `proxy_host`, clients use the API host |
| `expires_at` | optional; RFC 3339 |
| `created_at` | |

### `DiffResult` and `TableDelta`

`DiffResult` is `{"schema_diff": "<unified diff>", "tables": [TableDelta]}`.
Each `TableDelta` has `schema`, `table`, `base_rows`, `branch_rows`, `delta`,
optional `rows_unknown` and optional `sample_rows` (with `?data=N`).

- Row counts are planner estimates (`pg_class.reltuples`). A table never
  analyzed on a side is counted exactly with `count(*)` when its heap is
  64 MiB or less; otherwise that side is `-1`, `rows_unknown` is `true` and
  `delta` is `0` (the CLI and the PR comment show `?`).
- Sampling checks the branch's highest primary keys (up to
  `max(10 × N, 1000)`) against the base, so new rows with low or random keys
  (random UUIDs, say) in a large table can be missed. Tables without a
  primary key, or whose sampling fails, are skipped.
- The base is the state a reset would return the branch to: the fork point on
  the overlay backend, but for a zfs or csi branch created from another
  branch, the parent's **current** state. On csi a diff of such a branch is
  refused once its parent has been destroyed.

### Others

- `MaskScript`: `{"name": "...", "sql": "..."}`, applied in order inside every
  new or reset branch; `sql` must not be empty.
- `Transition`: `from_state`, `to_state`, `reason`, `actor`, `at`. The actor
  is `name (role)` for a stored token, `root (admin)` for `PGOVERLAY_TOKEN`,
  `local:<os user>` for local-mode `pgb`, or `system:reconcile`.
- `ReconcilePlan`: `{"actions": [{"kind", "target", "reason"}]}`; `actions`
  is `null` when there is nothing to do. The kinds are listed in
  [Troubleshooting](troubleshooting.md#what-reconcile-does).
- `Token`: `name`, `role`, `created_at`; never the token itself. Token names
  are 1 to 63 lowercase letters, digits, `.`, `_` or `-`, starting with a
  letter or digit; `root` is reserved.

## Errors

Every non-2xx response has the body `{"error": "<message>"}`.

| Status | When |
|---|---|
| `400` | invalid JSON, an **unknown field** (for example `ttl` instead of `ttl_seconds`), trailing data, an invalid name, image, token name, version or seed setting, `pg_version` not matching the seeded data directory |
| `401` | no token, or an unknown one |
| `403` | the token's role is below the route's minimum; `--max-branches` reached; the parent's overlay layer chain is at `--max-layer-depth` |
| `404` | unknown source, branch or token; the message names it |
| `409` | the name is taken (the message names the holder's state), the branch is in the wrong state for the operation, a source still has live branches, a zfs parent has live clones, a csi child's parent is gone, or there is nothing to recover |
| `413` | request body over 1 MiB |
| `422` | the seed or a masking script failed; the message carries the tool's output (clipped to 2 KiB; the full text is in branchd's log, never the password) |
| `500` | an internal error; the body says only `internal server error` and the detail is logged |
| `503` | `not leader`, `shutting down`, or the leader lost its Lease mid-operation. The operation was rolled back; retry, ideally against the Service that routes to the leader |
| `504` | a branch operation ran past `--stuck-timeout` and was rolled back |

The Go client (`internal/apiclient`, used by `pgb`) retries `503` for every
method, `502`, `504` and connection resets for idempotent methods, and dial
failures, with jittered backoff over about eight seconds.

## Stability promise

The API is versioned in the path. From v1.0.0 `/v1` is a
**backward-compatibility promise**:

- **Additive only.** New endpoints, new optional request fields and new
  response fields may be added within `/v1`. Clients must ignore response
  fields they don't recognise.
- **No silent breaks.** A documented response field is never renamed or
  removed, an endpoint never changes its method or path, and a required
  request field is never added, without a `/v2`. Both versions would then be
  served during a deprecation window.
- **Enforced in CI.** `internal/api/compat_test.go` locks the JSON field set
  of every response type listed on this page (`Branch`, `Source`, `Token`,
  `CreateTokenResponse`, `MaskScript`, `Transition`, `Usage`, `ErrorResponse`,
  `DiffResult`, `TableDelta`, `ReconcilePlan`, `Action`); a rename or removal
  fails the build.

Status codes for new failure modes may be added. Request decoding is strict
(unknown fields are rejected), so a client written against a newer server
that sends a field an older server does not know gets a `400` from the older
server, not a silently ignored field.
