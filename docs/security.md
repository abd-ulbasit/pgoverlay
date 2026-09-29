# Security

pgoverlay copies production-shaped data into many short-lived Postgres
instances and hands out connections to them. This page says who can reach
what, what each component trusts, and how to harden a deployment. To report a
vulnerability, see [Reporting a vulnerability](#reporting-a-vulnerability) at
the end.

pgoverlay is a development and test tool. Its security goal is that a
deployment done by the checklist below does not widen access to the data it
copies: only the people and systems you give a token or a branch password can
reach a branch, and nothing done in a branch changes the source (branches run
on a read-only copy and never connect to it). It is not built to contain
hostile code running inside a branch.

## What there is to protect

- **The data in branches.** Every branch is a copy of the source as of its
  seed, unmasked unless the source has masking scripts
  ([`pgb source set-mask`](usage.md#1-local-development)).
- **The source database.** Seeding connects to it with a replication (or
  `pg_dump`) credential that can read everything.
- **Credentials pgoverlay holds.** API tokens, rotated branch passwords, the
  at-rest key, the GitHub webhook secret and App key.
- **The hosts.** branchd drives a container runtime, and branch containers run
  with added privileges on some backends.

## Components and who can reach them

### REST API (`--api-addr`, default `:7070`)

- Every `/v1` route needs `Authorization: Bearer <token>` and a minimum role
  (the [endpoint table](api.md#endpoints) lists them):
    - **viewer** reads sources, branches, history, usage and the reconcile
      plan;
    - **operator** creates, resets, recovers, destroys and diffs branches and
      applies reconcile;
    - **admin** manages sources, masking scripts and tokens.
- `PGOVERLAY_TOKEN` is the built-in admin token. branchd refuses to start
  without one or with one shorter than 16 characters; it is compared in
  constant time and never stored. It appears in the audit log as `root`.
- Stored tokens (`pgb token create NAME --role ROLE`) are kept as SHA-256
  digests and shown once. Names are lowercase letters, digits, `.`, `_` and
  `-`, and `root` is reserved.
- Unauthenticated: `/healthz`, `/readyz`, `/metrics` (aggregate counts and
  gauges, no names or secrets) and the static web UI files under `/ui/`. The
  UI calls the API with the token you paste, which it keeps in the tab's
  `sessionStorage`; it is served with a strict Content-Security-Policy and
  refuses to be framed.
- Plaintext unless branchd has `--api-tls-cert`/`--api-tls-key`. Clients
  trust a private CA with `PGOVERLAY_CA_CERT`; `PGOVERLAY_TLS_SKIP_VERIFY=1`
  disables verification and is ignored when a CA is configured.
- Request bodies are capped at 1 MiB, unknown JSON fields are rejected, and
  the server bounds header and body read times. There is **no rate limit on
  failed authentication**; with a 16-character floor on the admin token and
  128-bit generated tokens, guessing is impractical, but put a proxy with rate
  limiting in front if the API faces an untrusted network.
- Every state change is journaled with the actor: `name (role)` for stored
  tokens, `root (admin)` for the admin token, `local:<os user>` for local-mode
  `pgb`, and `system:reconcile` for the daemon itself. `pgb history NAME`
  shows a branch's trail, and it survives the removal of the source.

### Postgres router (`--pg-addr`, default `:6432`)

The router accepts connections **before any authentication**: it reads the
startup message, looks up the branch named after the `@` in the database
name, and relays bytes. Authentication is between the client and the branch's
own Postgres.

- Anyone who can open a TCP connection can try branch names. Unknown,
  not-ready and unreachable branches all get the same refusal
  (`pgoverlay: database not available`, SQLSTATE `3D000`), but a **ready**
  branch can be confirmed before authenticating, because its password
  challenge is relayed. Branch names are not secrets.
- Denial-of-service bounds: a client must send its first byte within 2 s and
  finish startup within 10 s; the backend must reach `ReadyForQuery` within
  30 s; at most 256 connections are handled at once and at most 64 per client
  IP may be in the startup phase; an idle session (no bytes either way) is
  closed after 15 minutes.
- Cancel requests are forwarded only to the backend of the session that holds
  the cancel key; unknown keys are dropped.
- Plaintext unless branchd has `--pg-tls-cert`/`--pg-tls-key`; branchd logs a
  warning when the router listens on a non-loopback address without TLS.
  Clients should use `sslmode=verify-full`: `prefer`, libpq's default, falls
  back to plaintext without telling you.

### Branch instances

- Each branch is a Postgres container (a pod on Kubernetes) started from the
  source's image. Its roles and passwords are the source's, unless branchd
  runs with `--rotate-branch-credentials`, which sets a new random password on
  the source's connection role in every branch. In the default inherit mode, a
  branch accepts the **production password** of that role, so a leaked branch
  DSN is a leaked production credential. A rotated password is set with an
  `ALTER ROLE` passed to `psql -c` inside the branch, so it is part of that
  exec's command line: Kubernetes API audit logs that record `pods/exec`
  requests contain it.
- On Docker, branch ports are published on `127.0.0.1` of the Docker host
  only. Reach them through the router from anywhere else.
- **Privileges depend on the backend.**

    | Runtime / backend | Branch container |
    |---|---|
    | Docker, overlay or zfs | `CAP_SYS_ADMIN`, `apparmor=unconfined`, Docker's default seccomp profile |
    | Kubernetes hostpath (overlay) | `CAP_SYS_ADMIN`, seccomp and AppArmor `Unconfined`, `hostPath` volumes, pinned to the storage node |
    | Kubernetes csi | no added capabilities, seccomp `RuntimeDefault`, `allowPrivilegeEscalation: false` |

    The overlay mount needs `CAP_SYS_ADMIN`; on Docker the zfs backend gets
    the same settings because the driver does not distinguish backends. Postgres
    itself runs as the unprivileged `postgres` user after the entrypoint has
    mounted the overlay, so the capability is not in its effective set.
    But anyone with a **superuser** role on a branch can run shell commands as
    that user inside the container (`COPY ... PROGRAM`), and any local
    privilege escalation from there lands in a container with `CAP_SYS_ADMIN`
    and no AppArmor profile, which makes escaping to the host much easier.
    Treat superuser on a branch as close to root on the Docker host or storage
    node, and do not run untrusted code in branches.
- **The lazyrw shim** (overlay backend, on by default) is an `LD_PRELOAD`
  library that the branch entrypoint exports inside the branch container
  only; nothing on the host or in other containers loads it. Within the
  container it is active only in the `postgres` server process (it checks the
  program name and `PGDATA` once, at load): the entrypoint shell, `gosu`,
  `archive_command` and `COPY ... PROGRAM` children get pass-through wrappers.
  It changes how Postgres opens its own data files, adds no capability, and
  runs as the postgres user. It is installed from builds embedded in the
  pgoverlay binary and checked against their SHA-256 on the way in; a user
  who can write the branch's volume can replace it, but such a user can
  already change the branch's data and entrypoint. The builds are committed
  to the repository, and CI rebuilds them from source and fails on any byte
  of difference (see [SECURITY.md](https://github.com/abd-ulbasit/pgoverlay/blob/main/SECURITY.md#supply-chain-scanning)).
- pgoverlay never connects a branch to the source, but the network may allow
  it. On Docker, branch containers join the source's `--network` (or the
  default bridge), so code running in a branch can open connections to
  anything those containers can reach, the source included. On Kubernetes
  with `networkPolicy.enabled`, branch pods may only reach cluster DNS.
- Masking scripts run inside each new or reset branch before it is marked
  ready, so a masked source never serves unmasked data. They run on the branch,
  not the source: the seed volume itself holds unmasked data.

### Helper containers

One-shot helpers seed and settle sources, install entrypoints and the lazyrw
shim, measure disk usage, probe the copy-up mode at branchd startup and, in
Kubernetes hostpath mode or with a docker `--volume-root`, create and remove
volume directories.

- On Kubernetes every helper runs with `RuntimeDefault` seccomp and
  `allowPrivilegeEscalation: false`, except the zfs backend's, which are
  privileged and see `/dev/zfs`, and the copy-up probe, which mounts an
  overlay and so gets exactly a hostpath branch pod's settings (`SYS_ADMIN`,
  seccomp and AppArmor unconfined). In hostpath mode the file helpers run as
  root with the whole data root mounted.
- On Docker the copy-up probe likewise runs with a branch container's
  settings (`CAP_SYS_ADMIN`, `apparmor=unconfined`). With `--volume-root DIR`,
  the helpers that create and remove volume directories, and the one that
  sets the XFS extent size hint, run as root with `DIR` mounted.
- The settle helper runs the source's image as the postgres user, starts
  Postgres on pgoverlay's copy with a private socket and no TCP listener, and
  never connects to the source.
- The **source password** reaches a seed helper through its environment. On
  Docker, anyone who can run `docker inspect` on the host can read it while the
  helper exists. On Kubernetes it lives in a short-lived Secret, created just
  before the helper pod and deleted as soon as the container starts; an API
  server audit policy that logs Secrets at `Request` level or above records it.
  The password is never written to the registry.
- The utility helper image is pinned by digest; override it with
  `--kube-helper-image` for a mirror (pin that by digest too).

### The container runtime

`branchd` and local-mode `pgb` talk to the Docker API (or the Kubernetes API),
which is root-equivalent on a Docker host. Anyone who can run `pgb` in local
mode on a machine can do what the Docker socket allows. On Kubernetes, branchd
has a namespace-scoped Role: pods (create, delete, get, list, watch, exec,
log), Secrets (create and delete only, for helper environments), and, per
mode, PVCs and VolumeSnapshots (csi) or Leases and pod `patch` (leader
election). It never reads Secrets back.

### The storage node and volumes

- Overlay on Docker: the seed and every branch layer are Docker volumes under
  the engine's data root, or directories under `--volume-root` when it is
  set. Kubernetes hostpath: plain directories under `dataRoot` (default
  `/var/lib/pgoverlay`) on one node. Anyone with root on that host or node can
  read every branch and seed, unmasked seed included.
- csi: PVCs; access follows your storage system.

### XFS reflink hosts (CVE-2026-64600)

Where the volumes sit on XFS with `reflink=1` (branchd logs
`copy-up probe: mode=clone fs=xfs`), OverlayFS copy-up clones files, so every
file a branch writes shares blocks with the seed. CVE-2026-64600 ("RefluXFS",
published July 2026) is a race in the Linux XFS copy-on-write path, present
since 4.11: two concurrent `O_DIRECT` writes to a reflinked file can land in
the physical blocks of the file it was cloned from, so a local user can
overwrite a file they can only read. The fix was merged upstream on
2026-07-16 and distributions ship it as kernel updates.

pgoverlay does not create the bug: any local user or container with a
writable directory on such a filesystem can use it. But it matters here,
because code running in a branch (a superuser's `COPY ... PROGRAM`, as the
postgres user) can read the shared seed, and on an unpatched kernel could
write into it, changing the data every branch of that source reads. On XFS
reflink hosts (RHEL-family and Amazon Linux roots, or a `--volume-root` or
`dataRoot` on such a disk), run a kernel with the fix and reboot into it.
ext4, btrfs and XFS without reflink are not affected.

### The registry and the at-rest key

The state directory (`PGOVERLAY_HOME`, default `~/.pgoverlay`; in the Helm
chart `<dataRoot>/state` or the persistence PVC) holds:

- `pgoverlay.db`, the SQLite registry: sources' connection details (host,
  port, user, database; never the password), masking SQL, token digests,
  branch rows and the audit journal, and rotated branch passwords.
- `secret.key`, the 32-byte key that encrypts rotated branch passwords with
  AES-256-GCM, generated on first start unless you supply one
  (`PGOVERLAY_SECRET_KEY`, or `--secret-key-file` / `PGOVERLAY_SECRET_KEY_FILE`).

branchd creates the directory `0700` and the registry and key files `0600`,
and tightens older installs. A copy of the registry alone reveals no branch
password; a copy of the whole state directory does, so keep the key elsewhere
(`PGOVERLAY_SECRET_KEY` from a secret manager) if backups of the state
directory are widely readable. Passwords are cleared from a branch's row when
it is destroyed. If the key is lost, affected branches keep working and report
`password_unavailable`; a reset mints a new password. Rotate the key by
setting a new one and passing the old one in `PGOVERLAY_SECRET_KEY_PREVIOUS`
for one start. `PGOVERLAY_TOKEN` can be rotated at any time without touching
stored passwords.

### Seed credentials

`--via basebackup` needs a role with `REPLICATION`, which can read the whole
cluster; `--via dump` needs a role that can read what it dumps. branchd holds
the password only for the duration of the seed (and a refresh asks for it
again). Seed connections use `PGOVERLAY_SEED_SSLMODE` (default `prefer`);
across any network you do not control, set `verify-full` or at least
`require`.

### GitHub webhook service (`pgoverlay-github`)

- Deliveries are authenticated by HMAC-SHA256 over the raw body
  (`X-Hub-Signature-256`) with a secret of at least 16 characters.
  `GHOOK_REPOS` restricts which repositories can drive it; without it, any
  repository that knows the secret is accepted (a warning is logged).
- Each delivery id is processed once; with GitHub credentials, a `closed`
  delivery for a pull request GitHub reports open is ignored.
- It needs an **operator** token for branchd (the Helm chart falls back to the
  admin token until `ghook.apiTokenSecret` is set, and its NOTES warn about
  it). Its pod mounts no ServiceAccount token.
- Anyone who can open a pull request in an allowed repository causes a
  branch of the source to be created, and the PR comment shows the router
  address and the branch's database name (never a password). On a public
  repository, mask the source and use rotated credentials, or keep the router
  unreachable from the internet.
- Public commit-status text never includes internal errors; they read "see
  the pgoverlay-github logs (delivery <id>)".

## Hardening checklist

Everywhere:

- [ ] Generate `PGOVERLAY_TOKEN` with `openssl rand -hex 16` or longer, keep it
      in a secret store, and use it only to mint scoped tokens: `operator`
      for CI and ghook, `viewer` for dashboards and connect helpers.
- [ ] Mask the source (`pgb source set-mask`) before anyone outside the data's
      normal audience can reach a branch. Remember the seed itself is unmasked.
- [ ] Turn on `--rotate-branch-credentials` whenever branches are reachable
      from beyond the machines that already hold the production password.
- [ ] Serve the API and the router over TLS (`--api-tls-*`, `--pg-tls-*`),
      and have clients verify (`PGOVERLAY_CA_CERT`, `sslmode=verify-full`).
- [ ] Bind `--api-addr` and `--pg-addr` to the interfaces that need them, and
      firewall the rest; both default to all interfaces.
- [ ] Seed with a dedicated role limited in `pg_hba.conf` to the branchd
      host, preferably from a standby, with `PGOVERLAY_SEED_SSLMODE=verify-full`.
- [ ] Bound what clients can create: `--default-ttl`, `--max-ttl`,
      `--max-branches`.
- [ ] Back up `secret.key` with the registry, or supply the key from a secret
      manager.
- [ ] Run branchd on a host (or in a namespace) dedicated to it, and treat
      superuser on a branch as privileged access to that host.
- [ ] Alert on the [metrics](observability.md), and review `pgb history` for
      unexpected actors.
- [ ] Where branch volumes sit on XFS with `reflink=1`, run a kernel with
      the fix for [CVE-2026-64600](#xfs-reflink-hosts-cve-2026-64600).

On Kubernetes, additionally:

- [ ] Prefer `storage.mode=csi`: branch pods get no added capabilities and the
      namespace can enforce Pod Security `baseline`. Hostpath needs
      `privileged`.
- [ ] Set `networkPolicy.enabled=true` with `allowedClients`, `metricsFrom`
      and `sourceEgress`, so only seed helpers can reach the source.
- [ ] Pre-create Secrets and use `existingSecret` / `ghook.existingSecret`;
      `--set token=` stores secrets in the Helm release history.
- [ ] Give ghook an operator token (`ghook.apiTokenSecret`) and set
      `ghook.repos`.
- [ ] For a `LoadBalancer`, set `proxy.service.loadBalancerSourceRanges` and
      `ghook.service.loadBalancerSourceRanges` (GitHub's hook ranges), or make
      the load balancer internal.
- [ ] Check that your API server audit policy logs Secrets at `Metadata`
      level only, or accept that seed passwords appear in the audit log.
- [ ] Use the namespaced `deploy/preview-deployer-rbac.yaml` for CI instead
      of a cluster-admin kubeconfig, in a namespace dedicated to pgoverlay
      (its Helm storage access can read every Secret in that namespace).

## Known limitations

- The router cannot hide whether a ready branch exists (see above). A fix
  would need the router to take part in authentication; it is on the roadmap.
- No rate limiting of failed API authentication.
- Branch containers on Docker and Kubernetes hostpath hold `CAP_SYS_ADMIN`.
- In inherit mode a branch accepts the source's passwords.
- On Docker, the seed password is visible to `docker inspect` while the seed
  runs.
- A rotated branch password travels in the command line of the exec that sets
  it (visible in `pods/exec` audit logs on Kubernetes).
- The lazyrw shim and `pgoverlay-du` ship as prebuilt binaries committed to
  the repository and embedded in the pgoverlay binaries, not built on your
  machine. CI rebuilds them from source and fails on any difference, and
  pgoverlay checks each against its committed SHA-256 before use.

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability
reporting:
[**Report a vulnerability**](https://github.com/abd-ulbasit/pgoverlay/security/advisories/new)
(Security tab, then Advisories). Please do not open a public issue, pull
request or discussion for an undisclosed vulnerability.

Say which component is affected (`pgb`, `branchd`, the REST API, the Postgres
router, `pgoverlay-github`, the Helm chart, an SDK or the GitHub Action), the
version or commit (`pgb version`, `branchd -version`), how to reproduce it,
and what an attacker gains. The fix is developed in the private advisory and
released, and then the advisory is published, with a CVE where one applies,
crediting you unless you ask otherwise. [SECURITY.md](https://github.com/abd-ulbasit/pgoverlay/blob/main/SECURITY.md)
has the supported versions, release verification and supply-chain policy.
