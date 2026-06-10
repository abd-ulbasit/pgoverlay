# Branch per pull request (GitHub webhook)

`pgbranch-github` (Helm: the `ghook` sub-deployment) is a small webhook
service that gives every pull request its own Postgres branch:

| PR event | pgbranch action |
|---|---|
| opened / reopened | create branch `pr-<number>` from the configured source (no-op if it exists) |
| synchronize (push) | ensure the branch exists; reset it to the source snapshot only when `GHOOK_RESET_ON_PUSH=true` |
| closed (incl. merged) | destroy the branch (already-gone is fine) |

When a GitHub token is configured, the service also posts **one**
connect-info comment per PR (it marks the comment with `<!-- pgbranch -->`
and skips PRs that already have it).

## Example flow

1. Developer opens PR #42 against `acme/widgets`.
2. GitHub delivers a signed `pull_request` webhook to `POST /webhook`.
3. The service verifies the HMAC signature, sees action `opened`, and asks
   branchd to create branch `pr-42` from source `main` with a 72h TTL.
4. It comments on the PR:
   `psql -h pg.example.com -p 30432 -U app -d 'appdb@pr-42'`.
5. Pushes to the PR leave the branch alone (or reset it with
   `GHOOK_RESET_ON_PUSH=true`).
6. Merging/closing the PR destroys the branch.

## Registering the webhook

Plain **repository (or organization) webhook** is the simplest setup:

1. Generate a secret: `openssl rand -hex 32`.
2. Repo → *Settings* → *Webhooks* → *Add webhook*:
   - **Payload URL**: `https://<your-endpoint>/webhook`
   - **Content type**: `application/json`
   - **Secret**: the generated value
   - **Events**: select individual events → only **Pull requests**
3. Give the service the same secret via `GHOOK_WEBHOOK_SECRET`.

The webhook endpoint must be reachable from GitHub: expose the ghook
Service via an Ingress/LoadBalancer, or use
[`smee.io`](https://smee.io)/`gh webhook forward` for local development.

Requests are authenticated by the HMAC signature (`X-Hub-Signature-256`,
verified over the raw body). Additionally restrict which repositories may
drive branches with `GHOOK_REPOS=owner/name,owner/other` — when unset, any
repository that knows the secret is accepted (the service logs a warning at
startup).

### As a GitHub App (alternative)

A GitHub App works too and gives commenting a bot identity:

- **Webhook URL / secret**: as above; subscribe to **Pull request** events.
- **Repository permissions**: *Pull requests: Read-only*; add
  *Issues: Read and write* if you want the connect-info comment (PR comments
  are issue comments).
- Use an installation access token (or a fine-grained PAT with the same
  permissions) as `GHOOK_GITHUB_TOKEN`. Commenting is optional: without a
  token the service only manages branches and logs that comments are off.

## Configuration reference (environment)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `GHOOK_LISTEN` | no | `:8080` | HTTP listen address (`POST /webhook`, `GET /healthz`) |
| `GHOOK_WEBHOOK_SECRET` | **yes** | — | HMAC secret shared with GitHub |
| `GHOOK_PGBRANCH_SERVER` | **yes** | — | branchd base URL, e.g. `http://pgbranch-api:7070` |
| `GHOOK_PGBRANCH_TOKEN` | no | — | branchd API bearer token |
| `GHOOK_SOURCE` | **yes** | — | pgbranch source to branch from |
| `GHOOK_TTL` | no | none | branch TTL as a Go duration, e.g. `72h` |
| `GHOOK_RESET_ON_PUSH` | no | `false` | reset the branch on every push (synchronize) |
| `GHOOK_REPOS` | no | allow all | comma-separated `owner/name` allow-list |
| `GHOOK_GITHUB_TOKEN` | no | — | enables the PR comment (issues write) |
| `GHOOK_GITHUB_API` | no | `https://api.github.com` | GitHub API base (GitHub Enterprise) |
| `GHOOK_PROXY_HOST` | no | — | `host[:port]` of the pgbranch proxy shown in comments |

## Running on Kubernetes (Helm)

The pgbranch chart ships the service as an optional sub-deployment
(`deploy/helm/pgbranch`, image `pgbranch/ghook` — `make docker-build-ghook`):

```sh
helm upgrade --install pgbranch deploy/helm/pgbranch \
  --set node=storage-1 --set token=$PGBRANCH_TOKEN \
  --set ghook.enabled=true \
  --set ghook.webhookSecret=$WEBHOOK_SECRET \
  --set ghook.githubToken=$GITHUB_TOKEN \
  --set ghook.source=main \
  --set ghook.repos=acme/widgets \
  --set ghook.proxyHost=pg.example.com:30432
```

It talks to branchd over the in-cluster `…-api` Service and reuses the
chart's API token Secret. Secrets can come from a pre-created Secret
instead (`ghook.existingSecret`, keys `webhook-secret` and optionally
`github-token`). See `ghook.*` in `values.yaml` for TTL, reset-on-push and
service type.
