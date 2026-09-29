# Reference

Every command, flag and environment variable of the three binaries, checked
against their `--help`. The REST API has its own page: [REST API](api.md).

- `pgb`: the CLI. Local mode drives Docker directly; server mode talks to a
  `branchd`.
- `branchd`: the daemon (REST API, Postgres router, reconcile loop).
- `pgoverlay-github`: the GitHub webhook service; its environment is in
  [GitHub App](github-app.md#configuration-reference-environment).

All three print their version with `pgb version` (or `pgb --version`),
`branchd -version` and `pgoverlay-github -version`; branchd and
`pgoverlay-github` also log it at startup. Release builds are stamped by the
release workflow; `make build` stamps from `git describe`, and
`make build VERSION=v1.2.3` overrides it.

## `pgb`

### Local mode and server mode

| | Local mode | Server mode |
|---|---|---|
| Selected by | no `--server` | `--server URL` or `PGOVERLAY_SERVER` |
| Runs the engine | in the `pgb` process, against Docker | in `branchd` |
| State | the registry in `PGOVERLAY_HOME` (default `~/.pgoverlay`) | branchd's |
| Auth | whoever can use the Docker socket | `PGOVERLAY_TOKEN` as a bearer token |
| TTL reaping | none on its own: run `pgb gc` | branchd's reconcile loop |

Never run local-mode commands against a `PGOVERLAY_HOME` that a running
branchd owns: the SQLite registry has one writer.

### Commands

| Command | What it does |
|---|---|
| `pgb source add NAME` | Register a source and seed it. Flags below. |
| `pgb source refresh NAME` | Re-seed into a new generation. Existing branches keep their snapshot; new branches see the new one. `--password-env`, `--no-password`. |
| `pgb source rm NAME` | Remove a source; refused while it has live branches. |
| `pgb source ls` | List sources. |
| `pgb source set-mask NAME FILE...` | Replace the source's masking SQL with the files, applied in argument order inside every new or reset branch. |
| `pgb source get-mask NAME` | Print the masking scripts in order. |
| `pgb source clear-mask NAME` | Remove the masking SQL. |
| `pgb branch create NAME --from SOURCE` | Create a branch from a source. `--ttl 24h` sets an expiry. |
| `pgb branch create NAME --from-branch PARENT` | Create a branch from another branch. |
| `pgb branch ls` | List branches. `--usage` adds a SIZE column (one helper container per branch). Local mode notes branches past their TTL. |
| `pgb branch reset NAME` | Discard the branch's writes and re-clone it from its base (new container and port). Also retries a `failed` branch. |
| `pgb branch recover NAME` | Restart a `failed` branch on its existing data, keeping its writes. |
| `pgb branch destroy NAME` | Destroy one branch (container and layer). Retries a branch stuck in `destroying`. |
| `pgb connect NAME` | Print connection URLs for a ready branch (see below). |
| `pgb diff NAME` | Schema diff and row-count changes against the branch's base. `--all` lists unchanged tables; `--data` samples up to `--sample` (default 20, at most 500) new rows per grown table. |
| `pgb history NAME` | The branch's audit trail: time, transition, actor, reason. |
| `pgb doctor` | Print the reconcile plan without changing anything. |
| `pgb gc` | Apply the reconcile plan. |
| `pgb token create NAME --role ROLE` | Mint an API token (`viewer`, the default, `operator` or `admin`); printed once. Admin only. |
| `pgb token ls` / `pgb token revoke NAME` | List (names and roles only) or revoke tokens. |
| `pgb version` | Print the version. |

`pgb doctor` and `pgb gc` take `--stuck-timeout` (default `10m`) in local
mode: the age past which a `creating` or `resetting` row counts as abandoned.

#### `pgb source add` flags

| Flag | Default | Meaning |
|---|---|---|
| `--host` | required | source host as reachable **from containers**; an empty or blank value is rejected |
| `--port` | `5432` | |
| `--user` | `postgres` | seed user: `REPLICATION` privilege for `basebackup`, any user that can read the data for `dump` |
| `--database` | `postgres` | database recorded for connection strings, and dumped with `--via dump` |
| `--pg-version` | `17` | the source's major, `14` to `18`. For `basebackup` it must equal the source's major (a mismatch fails the seed with the right version in the message); for `dump` it must be at least the source's major |
| `--via` | `basebackup` | `basebackup` (physical) or `dump` (logical, for managed Postgres) |
| `--dump-schema` | whole database | schema to dump, repeatable (`--via dump` only) |
| `--image` | `postgres:<pg-version>` | image for the seed helpers and every branch; must carry the source's extensions, locales and libc, e.g. `postgis/postgis:17-3.5` or `pgvector/pgvector:pg17` |
| `--network` | | Docker network the source is reachable on |
| `--password-env` | `PGPASSWORD` | environment variable holding the source password |
| `--no-password` | | connect without a password (trust, peer or certificate auth); exclusive with `--password-env` |

#### `pgb connect`

Local mode prints the branch's own URL, without a password in the default
inherit mode (set `PGPASSWORD` to the source's password). Server mode prints
two URLs: the branch's own Postgres, which only works from the branchd host
(Docker publishes branch ports on its `127.0.0.1`) or inside the cluster
(Kubernetes pod IPs), and the router URL (database `db@branch`), which is the
one to use from anywhere else. The router address comes from `--proxy-host` /
`--proxy-port`, else from what branchd advertises (`--advertise-proxy-addr`),
else the `--server` host and port 6432. With `--rotate-branch-credentials`
both URLs include the branch's password. `connect` refuses a branch that is
not ready, and one whose rotated password cannot be decrypted.

#### Exit codes

`pgb doctor` exits `0` when there is no drift, `1` when it found drift, and
`2` when it could not compute the plan (branchd unreachable, authentication
failed, invalid flags), so it can gate CI. Every other command exits `0` on
success and `1` on any error.

### Environment

| Variable | Used by | Meaning |
|---|---|---|
| `PGOVERLAY_SERVER` | server mode | branchd base URL (`http://` or `https://`, no query or fragment); same as `--server` |
| `PGOVERLAY_TOKEN` | server mode | API bearer token; `pgb` warns when it is unset |
| `PGOVERLAY_CA_CERT` | server mode | PEM file of a CA to trust for an `https://` server (private or self-signed) |
| `PGOVERLAY_TLS_SKIP_VERIFY` | server mode | `1` disables certificate verification. Insecure, last resort; ignored with a warning when `PGOVERLAY_CA_CERT` is set |
| `PGOVERLAY_HOME` | local mode | state directory (default `~/.pgoverlay`) |
| `PGOVERLAY_SECRET_KEY`, `PGOVERLAY_SECRET_KEY_FILE`, `PGOVERLAY_SECRET_KEY_PREVIOUS` | local mode | the at-rest key, read the same way branchd reads it, to decrypt rotated passwords; `pgb` never generates a key |
| `PGOVERLAY_SEED_SSLMODE` | local mode | `sslmode` of seed connections (default `prefer`) |
| `PGOVERLAY_LAZYRW`, `PGOVERLAY_WAL_RECYCLE` | local mode | `on` or `off`, as branchd's `--lazyrw` and `--wal-recycle`: how the branches `pgb` starts copy files up (default `on` for both) |
| `PGOVERLAY_SEED_SETTLE` | local mode | how `source add` and `source refresh` settle a new seed: `freeze` (default), `recover` or `off`; see [`--seed-settle`](#seed-settle) |
| `PGOVERLAY_VOLUME_ROOT` | local mode | directory on the Docker host to create volumes under, as branchd's `--volume-root`; use the same value as a branchd sharing the state directory |
| `PGPASSWORD` | `source add`, `source refresh` | the source password, unless `--password-env` names another variable |
| `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG` | local mode | the Docker endpoint, resolved like the `docker` CLI does, including a context's TLS material. `ssh://` endpoints are not supported: run branchd on the Docker host and use `--server`, or forward the socket (`ssh -NL /tmp/pgoverlay-docker.sock:/var/run/docker.sock HOST` and `DOCKER_HOST=unix:///tmp/pgoverlay-docker.sock`). Branch ports are published on the Docker host's `127.0.0.1`, so direct connection strings only work on that host |

The server-mode client retries `503` responses for every method, and `502`,
`504` and connection resets for idempotent ones, with jittered backoff over
about eight seconds, so a leader failover or a rolling restart is usually
invisible.

## `branchd`

`PGOVERLAY_TOKEN` is required and must be at least 16 characters
(`openssl rand -hex 16`). On `SIGINT`/`SIGTERM` branchd stops accepting
connections and new mutations, lets in-flight requests finish for
`--shutdown-timeout`, cancels and rolls back what is left, and only then
releases its leader Lease. Branch containers keep running: they are durable
state. A second signal exits immediately.

### Flags

| Flag | Default | Environment | Meaning |
|---|---|---|---|
| `--api-addr` | `:7070` | | REST API listen address (all interfaces by default) |
| `--api-tls-cert`, `--api-tls-key` | | | PEM certificate and key; TLS for the API when both are set |
| `--pg-addr` | `:6432` | | Postgres router listen address (all interfaces by default) |
| `--pg-tls-cert`, `--pg-tls-key` | | | PEM certificate and key; the router answers `SSLRequest` with `S` when set, `N` otherwise |
| `--advertise-proxy-addr` | `--pg-addr`'s port | | `host:port` clients use to reach the router, returned as `proxy_host`/`proxy_port` in branch responses and used by `pgb connect` |
| `--reconcile-interval` | `1m` | | reconcile tick: TTL reaping, drift repair, garbage collection. `--reap-interval` is a deprecated alias |
| `--stuck-timeout` | `10m` | | see [below](#stuck-timeout) |
| `--shutdown-timeout` | `1m` | `PGOVERLAY_SHUTDOWN_TIMEOUT` | how long in-flight requests get on shutdown before they are cancelled and rolled back; keep it below the pod's `terminationGracePeriodSeconds` minus about 20 s |
| `--rotate-branch-credentials` | off | | give every branch its own generated password (returned as `password`) instead of inheriting the source's |
| `--secret-key-file` | | `PGOVERLAY_SECRET_KEY_FILE` | file holding the at-rest key (32 bytes, hex or base64); see [the at-rest key](#the-at-rest-key) |
| `--max-branches` | `0` (unlimited) | `PGOVERLAY_MAX_BRANCHES` | cap on live branches; creates past it return `403` |
| `--default-ttl` | `0` (none) | `PGOVERLAY_DEFAULT_TTL` | TTL for branches created without one |
| `--max-ttl` | `0` (none) | `PGOVERLAY_MAX_TTL` | upper bound on any requested TTL; longer ones are capped |
| `--max-layer-depth` | `100` | `PGOVERLAY_MAX_LAYER_DEPTH` | overlay backend: cap on a branch's frozen layer chain; branching from a branch at the cap returns `403` (see [Troubleshooting](troubleshooting.md#layer-chains-and-max-layer-depth)) |
| `--lazyrw` | `on` | `PGOVERLAY_LAZYRW` | overlay backend: `on` preloads the lazyrw shim into branch Postgres, so a table file is copied into the branch on its first write, not when it is read; a branch whose kernel (below 4.19) or image cannot use it copies on open and says so ([Troubleshooting](troubleshooting.md#a-branch-copies-eagerly)); `off` always copies on open. Branches pick a change up when they next start; see [below](#copy-on-first-write-lazyrw) |
| `--wal-recycle` | `on` | `PGOVERLAY_WAL_RECYCLE` | overlay backend, experimental: `off` starts branch Postgres with `wal_recycle=off`, so a checkpoint removes a WAL segment that came from the seed instead of copying it up to rename it |
| `--seed-settle` | `freeze` | `PGOVERLAY_SEED_SETTLE` | how a new seed (source add or refresh) is prepared before branches start from it: `freeze`, `recover` or `off`; see [below](#seed-settle) |
| `--disk-root` | see [Observability](observability.md#metrics) | | path whose filesystem the `pgoverlay_disk_bytes_*` gauges measure |
| `--volume-root` | | `PGOVERLAY_VOLUME_ROOT` | docker runtime, overlay backend: create every pgoverlay volume as a local bind volume over `<dir>/<volume>` on the Docker host instead of in Docker's own volume store; see [the volume root](#the-volume-root) |
| `--xfs-cowextsize` | `16k` | | overlay backend: when copy-up clones on XFS, the copy-on-write extent size hint set on the volume root (`--volume-root`, or `--kube-data-root`); `0` leaves the filesystem default (128 KiB) |
| `--runtime` | `docker` | | `docker` or `kube` |
| `--cow` | `overlay` | | copy-on-write backend: `overlay`, `zfs` ([experimental](zfs.md)) or `csi` (forced by `--kube-storage csi`) |
| `--zfs-dataset` | | | dataset prefix pgoverlay owns, e.g. `tank/pgoverlay` (required with `--cow zfs`) |
| `--kube-storage` | `hostpath` | | `hostpath` (one storage node) or `csi` (PVC clones) |
| `--kube-node` | | | storage node (required with `--runtime kube --kube-storage hostpath`) |
| `--kube-data-root` | `/var/lib/pgoverlay` | | data root on the storage node (hostpath only) |
| `--kube-namespace` | `POD_NAMESPACE`, else `pgoverlay` | | namespace for branch and helper pods |
| `--kube-helper-image` | alpine pinned by digest | | image for file-level helper pods, e.g. a mirror |
| `--kubeconfig` | in-cluster, then `KUBECONFIG` / `~/.kube/config` | | |
| `--csi-storage-class` | | | StorageClass for pgoverlay PVCs; required with `--kube-storage csi`; must support PVC cloning, or snapshots with `--csi-snapshot-class` |
| `--csi-snapshot-class` | | | clone through VolumeSnapshot + restore instead of direct PVC clones |
| `--csi-volume-size` | `10Gi` | | size of every pgoverlay PVC |
| `--leader-elect` | off | | HA: contend for the `pgoverlay-branchd` Lease; kube runtime only ([High availability](ha.md)) |
| `-version` | | | print the version and exit |

#### Stuck timeout

`--stuck-timeout` (default `10m`) is the age past which reconcile treats work
as abandoned:

- a `creating` or `resetting` branch, or a `seeding` source, that has made no
  progress for this long is failed. Running operations heartbeat their rows
  every `min(stuck-timeout / 4, 30s)`, so a slow but live operation (a long
  masking script, a large first recovery, a seed whose
  [settle](#seed-settle) VACUUMs a large database) is never failed by
  reconcile; the timeout bounds time without progress, not total time;
- a branch stuck in `destroying` for this long is retried;
- volumes younger than this are never garbage-collected, and finished helper
  containers older than this are removed.

Separately, a branch operation started through the REST API is cancelled and
rolled back (`504`) if it runs longer than `--stuck-timeout` in total. The
`504` names the elapsed time and the limit. When a create or reset is
legitimately that slow (a long masking script is the usual case), raise
`--stuck-timeout` (Helm value `stuckTimeout`) above its run time; there is no
environment variable for it. Operations `pgb` runs in local mode have no such
bound, and neither do `source add` and `source refresh` through the API,
settle included: they only heartbeat.

#### Copy on first write (lazyrw)

On the overlay backend (Docker and Kubernetes hostpath) every branch's
Postgres runs with the lazyrw shim preloaded: it opens table and
transaction-status files read-only and reopens a file read-write on its
first write, so a read copies nothing into the branch and a write copies
the file it touches once ([how it works](concepts.md#copy-on-first-write-the-lazyrw-shim)).

- `--lazyrw=on` (default; `PGOVERLAY_LAZYRW`, Helm `cow.lazyrw: true`): each
  branch checks at start that its kernel re-targets read-only files after a
  copy-up (Linux 4.19 and later) and that the build for its image's libc and
  architecture loads into its `postgres`, and uses the shim only then; on
  PG 18 and later it also pins `io_method=worker`. Otherwise the branch
  copies on open, as with `off`, and logs a warning.
- `--lazyrw=off`: branches copy every relation file Postgres opens, reads
  included.
- A change reaches a branch when it next starts: a reset, a recover, or a
  restart by reconcile. The setting is passed in the branch's environment
  (`PGOVERLAY_LAZYRW`), so no reinstall is needed. `pgb` in local mode reads
  `PGOVERLAY_LAZYRW` too.
- What each branch runs in is in `/pgoverlay/rw/cow-mode` inside it (`lazyrw`,
  `eager` or `off`, then a detail line), in branchd's log when the branch
  becomes ready, and in the `pgoverlay_branch_cow_mode{mode}` gauge
  ([Observability](observability.md#metrics)). Branches created by a release
  before v1.0.0 count as `eager` until they are reset
  ([Upgrading](upgrading.md#copy-on-write)).
- The zfs and csi backends ignore it: their clones copy blocks.

`--wal-recycle=off` (experimental, `PGOVERLAY_WAL_RECYCLE`) starts overlay
branches with `wal_recycle=off`: a checkpoint then removes a WAL segment that
came from the seed instead of renaming it, which on OverlayFS copies it up
first.

#### Seed settle

A `pg_basebackup` copy is an online backup: every branch started from it
would replay the WAL streamed during the backup, and its pages carry the
source's unset hint bits, dead tuples and unfrozen transaction ids, so reads
in a branch write (hint bits, pruning, anti-wraparound autovacuum) and, on the
overlay backend, copy the touched files into the branch. `--seed-settle` does
that work once, in the seed:

- `freeze` (default): start the seed once in a helper on the branch image,
  which completes the backup's recovery; run `VACUUM (FREEZE, ANALYZE)` on
  every database; `CHECKPOINT`; stop it cleanly.
- `recover`: the same without the VACUUM. Branches start without WAL replay,
  but reads may still set hint bits.
- `off`: leave the seed as `pg_basebackup` wrote it.

`--via dump` seeds end with a clean shutdown anyway; with `freeze` their
helper runs the VACUUM first. The source is never touched. The settle server
listens on a private socket only and ignores the parts of the source's
configuration that cannot start in a throwaway container; see
[Troubleshooting](troubleshooting.md#seeding) for what can still fail it.

#### Copy-up mode

On the overlay backend branchd probes, once at startup and in the
background, what an OverlayFS copy-up costs on the filesystem that holds the
volumes: it copies a 64 MiB file up through an overlay across two temporary
volumes, in a helper with the privileges a branch container already has
(`CAP_SYS_ADMIN`). The result is logged (`copy-up probe: mode=clone fs=xfs
...`) and exported as `pgoverlay_cow_copyup_mode`:

- `clone`: the filesystem reflinks (XFS with `reflink=1`, btrfs). Copy-up
  clones extents in milliseconds, and the copy shares every block with the
  source until one is rewritten, which is block-level copy-on-write. Branch
  usage is counted as the bytes the branch owns alone (a small static tool,
  `pgoverlay-du`, reads the extent map), because `du` would count a cloned
  1 GiB segment with one changed page as 1 GiB.
- `copy`: copy-up copies data (ext4, XFS without reflink, most others);
  usage is `du -sb`.
- `unknown`: not probed yet, or the probe failed; usage is `du -sb`.

The mode changes what a branch's first write to a file costs, not whether
reads copy (they do not, with `--lazyrw=on`): a copy of up to 1 GiB in
`copy` mode, an extent clone and then the rewritten blocks in `clone` mode.
`pgb` in local mode does not probe, so `pgb branch ls --usage` there is
always `du -sb`, which over-counts on a filesystem that clones.

On XFS, branchd also sets a copy-on-write extent size hint
(`--xfs-cowextsize`, 16 KiB) on the volume root, so the first write to a
cloned block copies 16 KiB rather than 128 KiB. Volumes created afterwards
inherit it. Docker's own volume directories are never touched, so the hint
needs `--volume-root` on the docker runtime.

#### The volume root

By default the docker runtime keeps pgoverlay's volumes in Docker's volume
store (`/var/lib/docker/volumes`), on whatever filesystem that is: usually
ext4, where copy-up copies. `--volume-root DIR` (or `PGOVERLAY_VOLUME_ROOT`)
creates every volume instead as a local-driver bind volume over `DIR/<volume>`
(`type=none,o=bind,device=DIR/<volume>`): source generations, writable
layers, frozen layers and diff throwaways alike. Point it at a directory on an
XFS (`reflink=1`) or btrfs disk and branches get block-level copy-on-write
without moving Docker. Volumes are still named volumes, mounted, listed and
labelled by name; `docker volume ls` shows them.

- `DIR` is a path on the Docker host, which is not this machine when
  `DOCKER_HOST` points elsewhere (it is inside the VM for Docker Desktop,
  Colima and OrbStack). It must exist; branchd checks it at startup and never
  creates it, so a disk that failed to mount cannot turn into a directory on
  the root filesystem.
- pgoverlay creates and deletes the directories itself, in helper containers
  that mount `DIR`, and records each volume's labels in
  `DIR/<volume>/.pgoverlay-labels.json`. Removing a volume deletes its
  directory. A directory whose volume is gone (a removal cut short, or a
  `docker volume rm`) is found and deleted by reconcile.
- Set the same value for `pgb` in local mode (`PGOVERLAY_VOLUME_ROOT`); both
  share the registry and the volumes.
- Changing or dropping it later is safe: existing volumes stay where they
  were created, keep working, and are removed from there.
- On SELinux-enforcing hosts (RHEL, Fedora) containers may only use
  directories labelled for them: `chcon -Rt container_file_t DIR`, or a
  matching `semanage fcontext` rule.
- Keep `DIR` on one filesystem: an extent clone cannot cross filesystems, so
  copy-up between two of them copies. Across subvolumes of one btrfs
  filesystem copy-up still clones (measured on Linux 7.0), but keep the
  volume directories themselves plain directories, as pgoverlay creates them:
  in the #49 evaluation, a branch whose overlay lower and upper sat in
  different btrfs subvolumes did not start (the image entrypoint's `find`
  reported `File system loop detected`).
- Every volume create and remove runs one short helper container (about a
  second on a busy host), in addition to what the operation already does.

#### The at-rest key

With `--rotate-branch-credentials`, branch passwords are stored encrypted
(AES-256-GCM) under a dedicated key, independent of `PGOVERLAY_TOKEN`. branchd
takes it from `PGOVERLAY_SECRET_KEY` (hex or base64), else
`--secret-key-file` / `PGOVERLAY_SECRET_KEY_FILE`, else
`<state dir>/secret.key`, which it generates (mode `0600`) on first start and
logs the key id it uses. To rotate the key, start once with the new key and
the old one in `PGOVERLAY_SECRET_KEY_PREVIOUS` (comma-separated for several);
a generated `secret.key` left in the state directory is picked up as a
previous key automatically. Back the key up with the registry. Details in
[Security](security.md#the-registry-and-the-at-rest-key).

### Environment

| Variable | Meaning |
|---|---|
| `PGOVERLAY_TOKEN` | required admin bearer token, at least 16 characters |
| `PGOVERLAY_HOME` | state directory: registry and `secret.key` (default `~/.pgoverlay`; created `0700`) |
| `PGOVERLAY_SECRET_KEY` | the at-rest key itself; wins over the key file |
| `PGOVERLAY_SECRET_KEY_PREVIOUS` | retired keys, comma-separated, decrypt-only |
| `PGOVERLAY_SEED_SSLMODE` | `sslmode` of seed connections: `disable`, `allow`, `prefer` (default), `require`, `verify-ca` or `verify-full` |
| `PGOVERLAY_MAX_BRANCHES`, `PGOVERLAY_DEFAULT_TTL`, `PGOVERLAY_MAX_TTL`, `PGOVERLAY_MAX_LAYER_DEPTH`, `PGOVERLAY_SHUTDOWN_TIMEOUT`, `PGOVERLAY_SECRET_KEY_FILE`, `PGOVERLAY_LAZYRW`, `PGOVERLAY_WAL_RECYCLE`, `PGOVERLAY_SEED_SETTLE`, `PGOVERLAY_VOLUME_ROOT` | defaults for the flags above |
| `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG` | Docker endpoint (docker runtime), as for `pgb` |
| `POD_NAMESPACE`, `POD_NAME`, `PGOVERLAY_POD_NAME`, `PGOVERLAY_POD_UID` | set by the Helm chart: the namespace, the leader-election identity (and leader label), and the pod that owns helper pods |

## Helm chart values

The chart's [`values.yaml`](https://github.com/abd-ulbasit/pgoverlay/blob/main/deploy/helm/pgoverlay/values.yaml)
documents every value; [Kubernetes](kubernetes.md) explains the ones that
matter. The chart maps these to branchd flags: `reconcileInterval`,
`stuckTimeout`, `shutdownTimeout` (seconds; the pod's grace period is this
plus 30), `rotateBranchCredentials`, `cow.lazyrw` (`--lazyrw`, `true` or
`false`), `seedSettle`, `helperImage`, `dataRoot`, `node`,
`storage.*`, `proxy.tls.certSecret`, `replicaCount` and
`leaderElection.enabled`. It does not yet expose `--max-branches`,
`--default-ttl`, `--max-ttl`, `--max-layer-depth`, `--advertise-proxy-addr`,
`--api-tls-*`, `--xfs-cowextsize`, `--wal-recycle` or a way to inject
`PGOVERLAY_SECRET_KEY`; the at-rest key is generated on the state volume.
`dataRoot` is `--kube-data-root`: put it on XFS (`reflink=1`) or btrfs for
clone copy-up (`--volume-root` is docker-only).
