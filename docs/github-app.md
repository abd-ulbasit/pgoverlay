# Branch per pull request (GitHub App)

`pgoverlay-github` (Helm: the `ghook` sub-deployment) is a small webhook
service that gives every pull request its own Postgres branch:

| PR event | pgoverlay action |
|---|---|
| opened / reopened | create branch `gh-<repo-key>-pr-<number>` from the configured source (no-op if it exists and is ready) |
| synchronize (push) | ensure the branch exists; reset it to the source snapshot only when `GHOOK_RESET_ON_PUSH=true` |
| closed (incl. merged) | destroy the branch (already-gone is fine) |

With GitHub credentials configured, the service also reports back to the PR:

- **Commit status** (context `pgoverlay/branch`) on the PR head SHA:
  `pending` while the branch is being created or reset, then `success`
  ("branch gh-d782c8-pr-42 ready — connect via pg.example.com:30432") only
  once the branch is ready, or `failure`. CI jobs can gate on the status
  instead of polling the branch with psql retry loops. A failure
  description names the step that failed; branchd's own refusals (an
  invalid name, a quota) are shown as they are, while anything else, such
  as a network error that would reveal branchd's in-cluster address on a
  public repository, reads "… failed; see the pgoverlay-github logs
  (delivery <id>)" and the detail is in the service log under that
  `X-GitHub-Delivery` id.
- **Live comment**: one marker comment (`<!-- pgoverlay -->`) per PR, kept
  current in place on every event — branch name, state (creating /
  resetting → ready / reset @ short-sha / failed / destroyed), the psql
  connect string, and the expiry when a TTL is set. Without
  `GHOOK_PROXY_HOST` the comment names the proxy database instead of a
  psql command. On close it is rewritten to say the branch was destroyed.
- **Diff comment** (opt-in, `GHOOK_DIFF_ON_PUSH=true`): after the branch
  is ready on opened/synchronize, a second marker comment
  (`<!-- pgoverlay-diff -->`) shows the schema diff against the branch's
  base (up to 3000 characters) and the tables whose estimated row count
  changed (up to 100). It is upserted separately and never overwrites the
  live comment.

## Branch names

| `GHOOK_BRANCH_NAMING` | Branch | Example (`acme/widgets`) |
|---|---|---|
| `pr-number` (default) | `gh-<repo-key>-pr-<number>` | PR #42 → `gh-d782c8-pr-42` |
| `git-branch` | `gh-<repo-key>-<sanitized head branch>` | `feat/Login` → `gh-d782c8-feat-login` |

- `<repo-key>` is the first six hex characters of the SHA-256 of the
  lowercased `owner/name`. It keeps pull requests of different
  repositories apart: PR #7 of `acme/widgets` and PR #7 of `acme/gadgets`
  get different branches, and closing one can never destroy the other's.
- `git-branch` sanitizing: ASCII letters are lowercased, letters and digits
  are kept, every other run of characters becomes one dash. The name is
  limited to 41 characters, which leaves 31 for the ref; a longer ref is cut
  to 24 characters and suffixed with six hex characters of its SHA-256, so
  two long refs that share a start (dependabot's, say) stay distinct.
- `git-branch` falls back to the `pr-number` name for pull requests from
  forks (a fork's branch name is chosen outside the repository and must not
  be able to land on the database of one of your branches) and for refs with
  no letters or digits.
- In `git-branch` mode, two open pull requests from the same head branch
  share one database, and closing either destroys it.
- All names start with `gh-`. Don't create `gh-` branches by hand: the
  service adopts a branch that already has its name.

Apps and CI don't need to ask the service for the name; they derive it the
same way. The connect helpers do it for you — `pgoverlayconnect` (Go) with
`Options{Repo, PR}` or `Options{Repo, Ref}`, and `pgoverlay-connect` (npm)
with `{ repo, pr }` or `{ repo, ref }` — and both are tested against the same
table as the service (`pgoverlayconnect/testdata/branch_names.json`). In a
shell, the `pr-number` name is:

```sh
key=$(printf '%s' "$GITHUB_REPOSITORY" | tr 'A-Z' 'a-z' | sha256sum | cut -c1-6)
branch="gh-$key-pr-$PR_NUMBER"
```

Renaming or transferring a repository changes its key: branches of pull
requests that were open at the time are not destroyed by their close event
and linger until their TTL.

**Upgrading from a build before 1.0.** Earlier builds named branches
`gh-pr-<n>` / `gh-<ref>`, without the repository key. After the upgrade the
service uses the new names: the next push to an open pull request creates
its branch under the new name, and closing that pull request does not
destroy the old one. Destroy the old branches with `pgb branch destroy
gh-pr-<n>` (or let their TTL expire), and update anything that hard-codes a
branch name.

## Delivery handling

- The service acknowledges each delivery at once (GitHub gives up after
  about ten seconds; creating a branch can take longer) and runs the branch
  operation in the background, bounded to five minutes.
- Deliveries for the same branch run one at a time, in the order they
  arrived: a push that lands while the branch is still being created waits
  for the create instead of racing it. Different branches run in parallel.
- A branch another operation is still creating, resetting or destroying is
  waited for. A branch whose create or reset failed is destroyed and
  created again by the next event.
- Each `X-GitHub-Delivery` id runs once: a delivery retried by GitHub or a
  proxy, or redelivered from the App's settings, is acknowledged (`200`,
  status `duplicate`) and ignored. The last 4096 ids are remembered, in
  memory.
- With GitHub credentials, a `closed` delivery for a pull request that
  GitHub reports open again (a stale redelivery, or a replayed request) is
  ignored instead of destroying the branch the open pull request uses.

## Example flow

1. Developer opens PR #42 against `acme/widgets`.
2. GitHub delivers a signed `pull_request` webhook to `POST /webhook`.
3. The service verifies the HMAC signature, acks the delivery, sets the
   `pgoverlay/branch` status to `pending`, and asks branchd to create branch
   `gh-d782c8-pr-42` from source `main` with a 72h TTL.
4. When the branch is ready the status flips to `success` and the PR
   comment shows
   `psql -h pg.example.com -p 30432 -U app -d 'appdb@gh-d782c8-pr-42'`.
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
   `openssl rand -hex 32` value (the service refuses secrets shorter than 16
   characters).
2. **Repository permissions**:
   - *Pull requests*: **Read-only** (the webhook payloads, and the
     open/closed check before a destroy)
   - *Commit statuses*: **Read and write** (the `pgoverlay/branch` status)
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
GHOOK_APP_PRIVATE_KEY_FILE=/etc/pgoverlay/app.pem   # or GHOOK_APP_PRIVATE_KEY with the PEM inline
GHOOK_WEBHOOK_SECRET=<the webhook secret>
```

Per delivery, the service reads the installation id from the webhook
payload, signs a short-lived RS256 app JWT with the private key, and
exchanges it for an installation token (cached until shortly before
expiry). No tokens to provision or rotate.

The webhook endpoint must be reachable from GitHub: expose the ghook
Service via an Ingress/LoadBalancer, or use
[`smee.io`](https://smee.io)/`gh webhook forward` for local development.
The service bounds every request itself (headers 10s, whole request 30s,
body 1 MiB), and an Ingress or load balancer in front can add its own
limits and TLS.

## Quick path: repository webhook + PAT

For a single repo or a first try, a plain webhook plus a token works too:

1. Repo → *Settings* → *Webhooks* → *Add webhook*: payload URL
   `https://<your-endpoint>/webhook`, content type `application/json`, a
   generated secret of at least 16 characters, and only **Pull requests**
   events.
2. Set `GHOOK_WEBHOOK_SECRET` to the same secret.
3. Set `GHOOK_GITHUB_TOKEN` to a fine-grained PAT with *Pull requests:
   read*, *Commit statuses: write*, *Issues: write* on the repo. Without a
   token the service still manages branches — it just can't comment or set
   statuses.

Requests are authenticated by the HMAC signature (`X-Hub-Signature-256`,
verified over the raw body). Additionally restrict which repositories may
drive branches with `GHOOK_REPOS=owner/name,owner/other` — when unset, any
repository that knows the secret is accepted (the service logs a warning at
startup). Several repositories can share one service: their branch names
differ by the repository key.

## Configuration reference (environment)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `GHOOK_LISTEN` | no | `:8080` | HTTP listen address (`POST /webhook`, `GET /healthz`) |
| `GHOOK_WEBHOOK_SECRET` | **yes** | — | HMAC secret shared with GitHub; at least 16 characters (startup fails otherwise) |
| `GHOOK_PGOVERLAY_SERVER` | **yes** | — | branchd base URL, e.g. `http://pgoverlay-api:7070` |
| `GHOOK_PGOVERLAY_TOKEN` | no | — | branchd API bearer token (operator role: it creates, resets and destroys branches) |
| `GHOOK_SOURCE` | **yes** | — | pgoverlay source to branch from |
| `GHOOK_APP_ID` | no | — | GitHub App id (App auth; needs the private key) |
| `GHOOK_APP_PRIVATE_KEY` | no | — | App private key PEM, inline |
| `GHOOK_APP_PRIVATE_KEY_FILE` | no | — | path to the App private key PEM (exclusive with the inline form) |
| `GHOOK_GITHUB_TOKEN` | no | — | PAT auth; mutually exclusive with `GHOOK_APP_ID` |
| `GHOOK_TTL` | no | none | branch TTL as a Go duration, e.g. `72h` |
| `GHOOK_RESET_ON_PUSH` | no | `false` | reset the branch on every push (synchronize) |
| `GHOOK_DIFF_ON_PUSH` | no | `false` | post the schema/data diff comment after opened/synchronize (needs GitHub credentials) |
| `GHOOK_BRANCH_NAMING` | no | `pr-number` | `pr-number` or `git-branch`; see [Branch names](#branch-names) |
| `GHOOK_REPOS` | no | allow all | comma-separated `owner/name` allow-list |
| `GHOOK_GITHUB_API` | no | `https://api.github.com` | GitHub API base (GitHub Enterprise) |
| `GHOOK_PROXY_HOST` | no | — | `host[:port]` of the pgoverlay proxy shown in comments/statuses; unset, the comment shows the proxy database without a psql command |

Comments and statuses require either App auth (`GHOOK_APP_ID` +
`GHOOK_APP_PRIVATE_KEY`/`_FILE`) or a PAT — never both. With neither, only
branch operations run.

## Running on Kubernetes (Helm)

The pgoverlay chart ships the service as an optional sub-deployment
(`deploy/helm/pgoverlay`, image `ghcr.io/abd-ulbasit/pgoverlay-ghook` — `make docker-build-ghook`):

```sh
helm upgrade --install pgoverlay deploy/helm/pgoverlay \
  --set node=storage-1 --set token=$PGOVERLAY_TOKEN \
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
TTL, reset-on-push, diff-on-push, branch naming and service type.
