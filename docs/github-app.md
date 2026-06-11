# Branch per pull request (GitHub App)

`pgbranch-github` (Helm: the `ghook` sub-deployment) is a small webhook
service that gives every pull request its own Postgres branch:

| PR event | pgbranch action |
|---|---|
| opened / reopened | create branch `pr-<number>` from the configured source (no-op if it exists) |
| synchronize (push) | ensure the branch exists; reset it to the source snapshot only when `GHOOK_RESET_ON_PUSH=true` |
| closed (incl. merged) | destroy the branch (already-gone is fine) |

With GitHub credentials configured, the service also reports back to the PR:

- **Commit status** (context `pgbranch/branch`) on the PR head SHA:
  `pending` while the branch is being created or reset, then `success`
  ("branch pr-42 ready — connect via pg.example.com:30432") or `failure`
  (the error, truncated to 140 chars). CI jobs can gate on the status
  instead of polling the branch with psql retry loops.
- **Live comment**: one marker comment per PR, updated in place — branch
  name, state (creating → ready / reset @ short-sha / destroyed), psql
  connect string, and the expiry when a TTL is set. On close it is rewritten
  to say the branch was destroyed.

## Example flow

1. Developer opens PR #42 against `acme/widgets`.
2. GitHub delivers a signed `pull_request` webhook to `POST /webhook`.
3. The service verifies the HMAC signature, acks the delivery, sets the
   `pgbranch/branch` status to `pending`, and asks branchd to create branch
   `pr-42` from source `main` with a 72h TTL.
4. When the branch is ready the status flips to `success` and the PR
   comment shows
   `psql -h pg.example.com -p 30432 -U app -d 'appdb@pr-42'`.
5. Pushes to the PR leave the branch alone (or reset it with
   `GHOOK_RESET_ON_PUSH=true` — the comment then shows `reset @ <sha>`).
6. Merging/closing the PR destroys the branch and updates the comment.

## Setup as a GitHub App (recommended)

A GitHub App is the right shape for this service: comments and statuses get
a bot identity, the private key never expires (unlike PATs), and the service
mints short-lived installation tokens itself.

Create the App manually (org or user → *Settings* → *Developer settings* →
*GitHub Apps* → *New GitHub App*):

1. **Webhook**: activate it, set the **Webhook URL** to
   `https://<your-endpoint>/webhook` and the **Webhook secret** to a fresh
   `openssl rand -hex 32` value.
2. **Repository permissions**:
   - *Pull requests*: **Read-only** (the webhook payloads)
   - *Commit statuses*: **Read and write** (the `pgbranch/branch` status)
   - *Issues*: **Read and write** (PR comments go through the issues API)
3. **Subscribe to events**: **Pull request**.
4. After creation: note the **App ID**, then **generate a private key**
   (GitHub downloads a PKCS#1 PEM; PKCS#8 works too).
5. **Install the App** on the repositories that should get branches.

Configure the service with the App ID and key — `GHOOK_GITHUB_TOKEN` must
stay unset (the two auth modes are mutually exclusive; startup fails if both
are present):

```sh
GHOOK_APP_ID=12345
GHOOK_APP_PRIVATE_KEY_FILE=/etc/pgbranch/app.pem   # or GHOOK_APP_PRIVATE_KEY with the PEM inline
GHOOK_WEBHOOK_SECRET=<the webhook secret>
```

Per delivery, the service reads the installation id from the webhook
payload, signs a short-lived RS256 app JWT with the private key, and
exchanges it for an installation token (cached until shortly before
expiry). No tokens to provision or rotate.

The webhook endpoint must be reachable from GitHub: expose the ghook
Service via an Ingress/LoadBalancer, or use
[`smee.io`](https://smee.io)/`gh webhook forward` for local development.

## Quick path: repository webhook + PAT

For a single repo or a first try, a plain webhook plus a token works too:

1. Repo → *Settings* → *Webhooks* → *Add webhook*: payload URL
   `https://<your-endpoint>/webhook`, content type `application/json`, a
   generated secret, and only **Pull requests** events.
2. Set `GHOOK_WEBHOOK_SECRET` to the same secret.
3. Set `GHOOK_GITHUB_TOKEN` to a fine-grained PAT with *Pull requests:
   read*, *Commit statuses: write*, *Issues: write* on the repo. Without a
   token the service still manages branches — it just can't comment or set
   statuses.

Requests are authenticated by the HMAC signature (`X-Hub-Signature-256`,
verified over the raw body). Additionally restrict which repositories may
drive branches with `GHOOK_REPOS=owner/name,owner/other` — when unset, any
repository that knows the secret is accepted (the service logs a warning at
startup).

## Configuration reference (environment)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `GHOOK_LISTEN` | no | `:8080` | HTTP listen address (`POST /webhook`, `GET /healthz`) |
| `GHOOK_WEBHOOK_SECRET` | **yes** | — | HMAC secret shared with GitHub |
| `GHOOK_PGBRANCH_SERVER` | **yes** | — | branchd base URL, e.g. `http://pgbranch-api:7070` |
| `GHOOK_PGBRANCH_TOKEN` | no | — | branchd API bearer token |
| `GHOOK_SOURCE` | **yes** | — | pgbranch source to branch from |
| `GHOOK_APP_ID` | no | — | GitHub App id (App auth; needs the private key) |
| `GHOOK_APP_PRIVATE_KEY` | no | — | App private key PEM, inline |
| `GHOOK_APP_PRIVATE_KEY_FILE` | no | — | path to the App private key PEM (exclusive with the inline form) |
| `GHOOK_GITHUB_TOKEN` | no | — | PAT auth; mutually exclusive with `GHOOK_APP_ID` |
| `GHOOK_TTL` | no | none | branch TTL as a Go duration, e.g. `72h` |
| `GHOOK_RESET_ON_PUSH` | no | `false` | reset the branch on every push (synchronize) |
| `GHOOK_REPOS` | no | allow all | comma-separated `owner/name` allow-list |
| `GHOOK_GITHUB_API` | no | `https://api.github.com` | GitHub API base (GitHub Enterprise) |
| `GHOOK_PROXY_HOST` | no | — | `host[:port]` of the pgbranch proxy shown in comments/statuses |

Comments and statuses require either App auth (`GHOOK_APP_ID` +
`GHOOK_APP_PRIVATE_KEY`/`_FILE`) or a PAT — never both. With neither, only
branch operations run.

## Running on Kubernetes (Helm)

The pgbranch chart ships the service as an optional sub-deployment
(`deploy/helm/pgbranch`, image `pgbranch/ghook` — `make docker-build-ghook`):

```sh
helm upgrade --install pgbranch deploy/helm/pgbranch \
  --set node=storage-1 --set token=$PGBRANCH_TOKEN \
  --set ghook.enabled=true \
  --set ghook.webhookSecret=$WEBHOOK_SECRET \
  --set ghook.appId=12345 \
  --set-file ghook.appPrivateKey=app.pem \
  --set ghook.source=main \
  --set ghook.repos=acme/widgets \
  --set ghook.proxyHost=pg.example.com:30432
```

(PAT mode: replace the two `app*` values with
`--set ghook.githubToken=$GITHUB_TOKEN`. The chart refuses to render with
both set.)

It talks to branchd over the in-cluster `…-api` Service and reuses the
chart's API token Secret. Secrets can come from a pre-created Secret
instead (`ghook.existingSecret`, keys `webhook-secret` and optionally
`github-token` / `app-private-key`). See `ghook.*` in `values.yaml` for
TTL, reset-on-push and service type.
