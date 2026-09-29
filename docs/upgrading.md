# Upgrading to v1.0

What changes when you move a deployment from a `v1.0.0-rc` build to v1.0.0.
Coming from `pgbranch` (`v1.0.0-rc.3` or older)? That was a rename with no
automatic migration; read
[the pgbranch → pgoverlay rename](https://github.com/abd-ulbasit/pgoverlay/blob/main/SECURITY.md#the-pgbranch--pgoverlay-rename)
first.

## Before you upgrade

1. **Back up the state directory** (`PGOVERLAY_HOME`, in the chart
   `<dataRoot>/state` or the persistence PVC): `pgoverlay.db` and, if present,
   its `-wal` file. The registry migrates in place on the first start (schema
   v11 to v15; the new migrations only add indexes and columns), and an older
   binary refuses to open the upgraded file (`registry schema is newer than
   this build`), so the backup is your way back.
2. **Check `PGOVERLAY_TOKEN` is at least 16 characters.** branchd now refuses
   to start with a shorter one. Generate a new one with `openssl rand -hex 16`
   if needed, but see the next point.
3. **Do not change `PGOVERLAY_TOKEN` in the same restart as the upgrade** if
   you use `--rotate-branch-credentials`. Rotated passwords used to be
   encrypted under a key derived from the token; the first v1.0 start
   re-encrypts them under a new dedicated key (`secret.key` in the state
   directory, generated on that start) and needs the old token to read them.
   Change the token on a later restart; from then on it no longer affects
   stored passwords. Back up `secret.key` with the registry from now on.

## Copy-on-write

v1.0.0 changes what a branch copies on the overlay backend
([#49](https://github.com/abd-ulbasit/pgoverlay/issues/49)): reads copy
nothing, a write copies the file it touches once (only its changed blocks on
XFS or btrfs), and new seeds are settled. Nothing needs configuring, and no
registry migration is involved, but existing branches and seeds do not change
by themselves.

- **Existing branches keep copying eagerly.** A branch created by an earlier
  build has the old entrypoint in its writable layer, so its Postgres runs
  without the lazyrw shim and every table it opens is copied, as before.
  branchd reports it as `eager` in `pgoverlay_branch_cow_mode`, with the
  reason "the branch's entrypoint predates lazyrw" in its log. It moves to
  the shim only when something installs the current entrypoint (a restart
  by reconcile or by the Docker daemon keeps the old one, and nothing
  migrates branches automatically):
    - `pgb branch reset NAME` (discards the branch's writes);
    - `pgb branch recover NAME`, for a `failed` branch (keeps them);
    - creating a branch from it (`--from-branch`): the freeze restarts the
      parent on a fresh writable layer with the current entrypoint, and its
      data carries over in the frozen layer.
- **Existing seeds are not settled.** Branches of a seed taken before v1.0.0
  still replay the backup's WAL on first start, and their first reads set
  hint bits, which are writes: with the shim, those copy the segments they
  touch. `pgb source refresh NAME` takes a new, settled generation for new
  branches; existing branches keep the generation they were created from.
- **Seeding takes longer.** The settle adds roughly one read of the database
  plus a write of its unfrozen pages to every `source add` and `refresh`, in
  an extra helper on the branch image. It can fail a seed that used to
  succeed when the source's configuration cannot start in a container (an
  `include` of a file outside the data directory, a setting the image does
  not know); every branch of that seed would have failed the same way. See
  [Troubleshooting](troubleshooting.md#seeding). `--seed-settle=off` restores
  the old behaviour.
- **Diff row counts of seeded tables are estimates.** The settle runs
  `ANALYZE`, so a small table that was never analyzed on the source is no
  longer counted exactly; a small change to it shows once the branch has
  analyzed it ([usage](usage.md#5-reviewing-migrations-with-pgb-diff)).
  `--seed-settle=recover` keeps the old counting.
- **New settings**, all defaulting to the new behaviour (`pgb` in local mode
  reads the same environment variables; details in the
  [reference](reference.md#copy-on-first-write-lazyrw)):
    - branchd `--lazyrw=on|off` (`PGOVERLAY_LAZYRW`, Helm `cow.lazyrw`);
    - `--seed-settle=freeze|recover|off` (`PGOVERLAY_SEED_SETTLE`, Helm
      `seedSettle`);
    - `--volume-root` (`PGOVERLAY_VOLUME_ROOT`, docker only) and
      `--xfs-cowextsize`;
    - the experimental `--wal-recycle`.
- **New metrics:** `pgoverlay_branch_cow_mode{mode}` and
  `pgoverlay_cow_copyup_mode{mode}`; alert on `mode="eager"`
  ([Observability](observability.md)).
- **Privileges are unchanged.** Branch containers keep exactly the
  capabilities they had. branchd runs one more helper at startup, the copy-up
  probe, with a branch container's privileges, and it creates and removes two
  temporary `pgoverlay-probe-*` volumes (reconcile collects them if branchd
  dies first).
- **Kernel.** Copy-free reads need Linux 4.19 or later where branches run.
  On older kernels branches work as before and report `eager`.

## Behaviour changes

**GitHub App branch names.** Branches created by `pgoverlay-github` are now
named `gh-<repo-key>-pr-<number>` (or `gh-<repo-key>-<ref>` in `git-branch`
mode), where the repository key is six hex characters of the SHA-256 of the
lowercased `owner/name`. Branches named `gh-pr-<n>` by an earlier build are
not destroyed when their pull request closes; destroy them by hand or let
their TTL expire, and update anything that hard-codes a name. The connect
helpers derive the new names: `pgoverlayconnect` needs `Options{Repo, PR}` or
`Options{Repo, Ref}`, and `pgoverlay-connect` 0.2.0 (npm) needs `{repo, pr}`
or `{repo, ref}`. Details: [GitHub App](github-app.md#branch-names).

**REST API** (all additive for well-behaved clients, see
[REST API](api.md)):

- Request bodies are decoded strictly: an unknown field (a typo such as
  `ttl` for `ttl_seconds`) is a `400`, and bodies over 1 MiB are a `413`.
- `GET /v1/branches/{name}/diff` needs the operator role and the leader.
- New statuses: `422` for a failed seed or masking script (with the tool's
  output), `503` when leadership moves or branchd shuts down mid-operation,
  `504` when a branch operation outlives `--stuck-timeout`. All three mean the
  operation was rolled back.
- A failed destroy answers `409` (something still uses the branch's volume),
  `502` (the container runtime is unreachable) or `500` with the cause it
  journaled, instead of a bare `500`; the branch stays `destroying` until a
  destroy succeeds.
- Token names must be lowercase (`^[a-z0-9][a-z0-9._-]{0,62}$`) and `root` is
  reserved. Existing tokens keep working.
- New: `POST /v1/branches/{name}/recover`; `Branch.password_unavailable`,
  `Branch.proxy_host`/`proxy_port`, `Source.image` and
  `CreateSourceRequest.image`. Duplicate names return `409` naming the holder
  instead of SQLite's text, and not-found errors name the missing object.

**CLI.** `pgb source add` rejects an empty `--host`, `--user`, `--database`
or `--pg-version`, an out-of-range `--port`, and a `--via` other than
`basebackup` or `dump`. `--server` must be an `http(s)://` URL. `pgb doctor`
exits `2` (not `1`) when it cannot compute the plan. `pgb connect` refuses a
branch that is not ready. New commands: `pgb branch recover`,
`pgb source clear-mask`, `pgb version`; new flags: `--image` and
`--no-password` on `source add`, `--proxy-host`/`--proxy-port` on `connect`.

**Seeding.** A `basebackup` source whose major differs from `--pg-version` now
fails at seed time instead of producing branches that cannot start. Seeding
from a standby works (the recovery settings are stripped). Branches no longer
inherit the source's port, listen and socket settings, logging collector,
archiving or synchronous standby names ([Troubleshooting](troubleshooting.md#seeding)).

**Docker runtime.** Branch ports are chosen by pgoverlay and pinned, branch
containers restart with the daemon (`unless-stopped`), and reconcile restarts
branch containers that are removed or stopped. Execs inside branches run as
the `postgres` OS user. `ssh://` docker contexts are refused with an
explanation instead of failing obscurely.

**Web UI.** The token is kept per tab (`sessionStorage`), so paste it again in
a new tab; reset asks for confirmation.

**Helm chart.**

- `shutdownTimeout` (default 60 s) sets branchd's drain budget and the pod's
  `terminationGracePeriodSeconds` (the budget plus 30 s).
- With `replicaCount > 1` or `leaderElection.enabled`, the API Service routes
  to the leader through the `pgoverlay.leader` pod label, and the Role gains
  `pods patch`. The Role also gains `secrets create/delete` for helper
  environments.
- In csi mode with persistence, `node` is no longer needed; replicas
  co-locate through a pod affinity.
- `networkPolicy.sourceEgress` now applies to the seed helper pods; branch
  pods may only reach DNS.
- Give ghook an operator token with `ghook.apiTokenSecret`; the chart's
  NOTES warn while it falls back to the admin token.
- Label the namespace for Pod Security (`privileged` for hostpath, `baseline`
  for csi with persistence); the NOTES print the level.

**Images and the Action.** Release images are multi-arch (linux/amd64,
linux/arm64) from v1.0.0. The Action gains `proxy_host`, `proxy_port`,
`proxy_database`, `user` and (in rotation mode) `password` outputs and a
`proxy_host` input; connect through the `proxy_*` outputs. `action@v1` moves
to v1.0.0 when it is released.
