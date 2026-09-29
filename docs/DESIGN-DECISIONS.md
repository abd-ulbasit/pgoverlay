# Design decisions (ADRs)

Eleven decisions that shaped pgoverlay, each recorded in the same four parts:

- **Context** — the problem and the competing pressures.
- **Decision** — what was chosen, and where it lives in the code.
- **Alternatives considered** — the roads not taken, and why.
- **Consequences / trade-offs** — what this buys, and what it costs.

File paths are cited throughout so every claim is checkable against the source.
Where the implementation ended up subtler than the original intent, these
records follow the code.

Related: [architecture](architecture.md) describes what was built,
[code tour](code-tour.md) maps it package by package, and
[deep dives](deep-dives.md) covers the handful of places where the obvious
implementation turned out to be wrong.

---

## ADR-01: No Kubernetes operator, no CRDs — a plain daemon + CLI + Helm chart

**Context.** pgoverlay provisions stateful resources (volumes, Postgres
containers) and needs a reconcile/GC loop to converge reality with its
registry — exactly the workload the operator pattern was built for. The pull to
"just write a controller with a `Branch` CRD" is strong. But the engine must
also run on a laptop under Docker, with no apiserver in sight.

**Decision.** Ship `branchd`, a single daemon (`cmd/branchd/main.go`) exposing a
REST control plane (`--api-addr`, default `:7070`) and a Postgres router
(`--pg-addr`), driven by a CLI and a Helm chart (`deploy/helm/pgoverlay`).
pgoverlay defines no CustomResourceDefinitions: the only matches for
`grep -rl CustomResourceDefinition` are the vendored external-snapshotter CRDs
under `hack/csi/snapshotter/`, which the CSI integration tests install into a
kind cluster (and this sentence). The reconciliation
benefit is kept as an in-process loop: `Engine.RunReconcile` runs on a ticker
(`internal/engine/reconcile.go`) and computes a plan/apply diff just like a
controller's reconcile.

**Alternatives considered.** A controller-runtime operator with a `Branch` CRD
(state in etcd, `kubectl get branches`); this couples the tool to Kubernetes and
etcd and cannot run under Docker.

**Consequences / trade-offs.** Runs identically on Docker and K8s; no CRD
install, no etcd coupling, no RBAC for custom resources. The cost: no
`kubectl get branches` — branches are visible only via the REST API / CLI, and
you reimplement the plan→apply→re-check loop that controller-runtime would have
given for free (see ADR-08).

---

## ADR-02: SQLite as the registry (pure-Go, CGO-free)

**Context.** A *database*-branching tool needs to store its own metadata
somewhere. Requiring an external Postgres/etcd to run the thing that branches
Postgres is "turtles all the way down": more to deploy, more to back up, a
bootstrapping circularity.

**Decision.** Use SQLite via `modernc.org/sqlite` (pure Go, no CGO — `go.mod`,
`internal/registry/registry.go`), so `branchd` stays a single static binary. The
file is opened with `journal_mode(WAL)`, `busy_timeout(5000)` and
`foreign_keys(1)` pragmas and `_txlock=immediate` (every transaction takes the
write lock up front), and `db.SetMaxOpenConns(1)` serializes all writers
(`dsnParams` and `Open` in `registry.go`). Schema is versioned by
`PRAGMA user_version`: a `migrations` slice where entry *i* upgrades version
*i*→*i+1*, applied in a transaction that bumps the pragma
(`internal/registry/schema.go`, now through **v15** — the audit trail keeps
the names of removed sources' branches, and destroyed branches drop their
passwords). A binary refuses a registry whose version is newer than it knows
(`ErrSchemaTooNew`).

**Alternatives considered.** Postgres (the bootstrapping problem above); etcd
(operational weight, another distributed system to run for single-node state).

**Consequences / trade-offs.** Zero-dependency single binary, trivially
backed up (copy a file), in-process transactions. Cost: single writer, single
node — the registry is not horizontally scalable. That constraint is *mitigated*
rather than removed by leader-election HA (ADR-07): replicas share one RWO
volume and only the leader writes.

---

## ADR-03: Saga pattern with compensations for create / reset / destroy

**Context.** Provisioning a branch is multi-step: insert a registry row, create
the writable layer, install the entrypoint, start the container, wait for
readiness, apply masking, rotate credentials, mark ready. Any step can fail. A
naive happy-path leaks containers and volumes on partial failure.

**Decision.** Implement each mutation as a saga: every step that creates a
resource pushes a compensation closure onto an `undo` stack, and a `fail` helper
runs the stack in reverse on any later error (`internal/engine/saga.go`,
`provision` / `provisionZFS`). The same pattern covers reset (re-clone) and
destroy. Compensations run on a `context.WithoutCancel(ctx)` background context
so cleanup still happens even if the request was cancelled.

**Alternatives considered.** Happy-path-only with a janitor to mop up later
(leaves orphans live longer, racy); a full workflow engine (overkill for ~6
steps).

**Consequences / trade-offs.** No orphaned containers/volumes on failure; the
state machine stays consistent. Cost: roughly double the code of a happy path,
and compensations are **best-effort** — a failing undo is logged via
`logCompensationErr` and surfaced on a metric rather than retried. The
backstop is the reconcile loop, which GCs anything a compensation missed
(ADR-08).

---

## ADR-04: A Postgres wire-protocol proxy for branch connections

**Context.** A client needs to reach *a specific branch's* Postgres. Each branch
is a separate instance on its own host:port. Exposing one port per branch
sprawls and breaks as branches churn; clients want one stable endpoint.

**Decision.** Run a Postgres wire-protocol router (`internal/pgproxy/proxy.go`).
Clients connect to one address with `database=dbname@branch`; the proxy reads
the startup message, splits off the `@branch` suffix, resolves the branch to its
backend address via the registry (`RegistryResolver`, ready-only), rewrites the
`database` param back to the real dbname, replays startup to the backend, and
then relays bytes transparently in both directions. Because it only relays,
**SCRAM/auth flows pass straight through untouched** — the proxy never sees
credentials. It answers `SSLRequest` with TLS upgrade when a `TLSConfig` is set,
else `'N'`.

**Alternatives considered.** Port-per-branch (sprawl, churn); DNS-per-branch
(needs DNS plumbing, still per-branch endpoints).

Because it relays the backend's startup response, it also sees each session's
`BackendKeyData` and forwards a `CancelRequest` to the backend holding that key
(`internal/pgproxy/cancel.go`), so query cancellation works through it.

**Consequences / trade-offs.** One stable endpoint, auth-transparent, branch
selection in the connection string (works with any Postgres client). Cost: the
proxy is an **unauthenticated routing surface** — anyone who can dial it can
attempt to route. Hardening in code: a uniform `genericRouteRefusal`, so an
unauthenticated client cannot tell "unknown" from "not-ready" from
"unreachable"; startup deadlines (first byte, whole startup, backend
`ReadyForQuery`), a `MaxConns` cap and a per-IP cap on connections still in
startup (fast-refuse, not queue), and an idle timeout against slow-loris/DoS.
The residual is honest: a *ready* branch can still be confirmed before
authenticating, because its auth challenge is relayed, and a blackholed
backend is refused only after the dial timeout. Hiding that would need the
proxy to take part in authentication (a synthetic SCRAM exchange), which is on
the roadmap. Production posture leans further on TLS, NetworkPolicy and
credential rotation.

---

## ADR-05: A `Driver` interface with Docker and Kubernetes implementations

**Context.** The branching logic (saga, layer planning, masking, readiness) is
identical whether a branch runs as a Docker container on a laptop or a pod in a
cluster. Baking runtime calls into the engine would fork that logic two ways.

**Decision.** Define a single `Driver` interface in `internal/runtime/runtime.go`
(`CreateVolume`, `CloneVolume`, `StartBranch`, `Exec`/`ExecOutput`, `Inspect`,
`StopRemove`, `ListManaged`, `ListManagedVolumes`, …) and implement it for
Docker (`docker.go`) and Kubernetes (`kube.go`, `kube_csi.go`). The engine
depends only on the interface — `cmd/branchd/main.go` selects the
implementation from `--runtime docker|kube`. Resource addressing is abstracted
via `Mount{Kind, Volume, Target}` (named volume vs. host path).

**Alternatives considered.** A Docker-only tool (no cluster story); separate
engines per runtime (duplicated, divergent branching logic).

**Consequences / trade-offs.** Same branching code on laptop and cluster; new
runtimes are additive. Cost: the interface is the lowest common denominator —
e.g. `Exec`/`ExecOutput` expose no stdin, which is why rotated passwords pass
through `psql -c` argv (a documented, bounded leak in `rotateBranchCredentials`,
saga.go) rather than stdin; widening it means touching both drivers.

---

## ADR-06: hostPath + in-container OverlayFS vs. CSI volume-snapshot clones

**Context.** This is the central Kubernetes trade-off. Copy-on-write branching
needs cheap clones of a source volume. Two mechanisms exist, with opposite
operational profiles.

**Decision.** Support **both**, selected by `--kube-storage hostpath|csi`
(`cmd/branchd/main.go`), and recommend CSI for production.
- **hostPath + overlay** (`internal/runtime/kube.go`, `hostPathStorage`):
  "volumes" are subdirectories of `--kube-data-root` on one designated storage
  node; every pod is pinned there via `nodeName`, and branch pods run with
  `SYS_ADMIN` to perform the in-container overlay mount. Universal and cheap but
  **single-node and privileged**.
- **CSI clones** (`internal/runtime/kube_csi.go`, `csiStorage`): branches are
  PVCs created with a `dataSource` clone of the source PVC — or, when a
  `--csi-snapshot-class` is set, a `VolumeSnapshot` + restore. Pods schedule on
  **any node with no extra capabilities**, but it needs a clone/snapshot-capable
  CSI driver.

**Alternatives considered.** Picking only one: overlay-only excludes multi-node
and privilege-averse clusters; CSI-only excludes clusters without a capable CSI
driver and the zero-dependency dev path.

**Consequences / trade-offs.** Maximum portability — runs on a bare node or a
managed cluster. The Helm values and `docs/kubernetes.md` spell out the security
posture: overlay needs a relaxed seccomp profile and a privileged kernel
capability pinned to one node (single-node / trusted scope), while CSI is the
recommended production default (multi-node, unprivileged). Cost: two storage
code paths to maintain and test.

---

## ADR-07: Leader-election HA with a leader-gated control plane

**Context.** You want multiple `branchd` replicas for availability, but the
SQLite registry is single-writer (ADR-02). Two replicas mutating it concurrently
would corrupt state.

**Decision.** Optional `--leader-elect` (kube only) makes replicas contend for a
`coordination.k8s.io` Lease named `pgoverlay-branchd`
(`internal/ha/leader.go`, client-go `leaderelection`). Only the leader runs the
reconcile loop and accepts **mutating** `/v1` requests; the API composes a
`LeaderGate` in front of every mutating route (`internal/api/leader.go` —
`mutate = requireRole` first, then `requireLeader`), returning **503 "not
leader"** on followers. Reads, `/healthz`, `/readyz`, `/metrics` and the proxy
serve from any replica. Every replica opens the same read-write registry; the
gate, not the handle, is what keeps followers from writing. Gaining the Lease
opens the gate, labels the leader's pod `pgoverlay.leader=true` (the chart's
API Service selects it, so clients reach the leader) and runs an immediate
reconcile to converge drift; losing it closes the gate, cancels the loop and
cancels every mutation admitted during the term, whose sagas roll back. The
gate defaults to `leader=true`, so with election **off** (Docker / single
instance) every node is always leader and mutations behave normally.

**Alternatives considered.** A distributed multi-writer store (defeats ADR-02);
active/active without coordination (registry corruption).

**Consequences / trade-offs.** Availability without giving up single-writer
safety; the proxy scales out. Cost: writes are not HA-scaled (only the leader
mutates), and an RWO state volume binds all replicas to one node (the chart
pins them with `nodeName` in hostpath mode, or co-locates them with a pod
affinity in csi mode; see [High availability](ha.md)).

---

## ADR-08: Instance-scoped reconcile and GC

**Context.** The reconcile loop reaps orphaned containers and volumes. On a
shared Docker daemon — or in the parallel integration-test suite — several
pgoverlay instances coexist. A naive "reap anything managed" would have one
instance destroy another's *live* resources.

**Decision.** Every managed resource is stamped with the owning registry's
instance id under the label `pgoverlay.instance`
(`runtime.LabelInstance`). `instanceLabels` in `internal/engine/saga.go` is the
single chokepoint that adds it, so no call site can forget. The instance id is a
stable value minted on first registry open and stored in the `meta` table
(schema v8). Reconcile filters strictly on it: `PlanReconcile`
(`internal/engine/reconcile.go`) skips any managed container whose
`pgoverlay.instance` label is absent or names another instance, and
`ListManagedVolumes(ctx, instanceID)` scopes volume GC the same way.

**Alternatives considered.** A dedicated daemon per host (operationally
heavier); reaping by `pgoverlay.managed=true` alone (cross-instance data loss).

**Consequences / trade-offs.** Safe multi-tenancy on one daemon and a safe
parallel test suite. Reconcile additionally re-checks every destructive action
against the live registry immediately before acting (`applyAction`), so a
resource claimed between plan and apply is spared. Cost: one more label to keep
consistent; the ZFS backend (which manages datasets, not driver volumes) returns
no volumes here and GCs via its own per-branch/source paths instead.

---

## ADR-09: Token auth — SHA-256-hashed tokens, roles, env bootstrap

**Context.** The REST API mutates infrastructure; it needs authn/authz. Storing
plaintext tokens is a liability, and there must be a way to bootstrap the first
admin before any token exists.

**Decision.** Bearer tokens with three ranked roles —
`viewer < operator < admin` (`internal/registry/tokens.go`). Only the
**SHA-256 hex digest** of a token is stored; the plaintext is shown once at
creation and is never recoverable. `LookupAPIToken` is an indexed point lookup
on `token_hash` (unique index `api_tokens_hash`, schema v10) — timing-safe by
construction because the discriminator is itself a hash of the secret. The
env var `PGOVERLAY_TOKEN` is the **admin bootstrap**: it is never stored, and is
matched in the middleware with a **constant-time compare**
(`crypto/subtle.ConstantTimeCompare`, `internal/api/middleware.go`,
`resolveActor`) under the `root` audit sentinel. `requireRole` returns 401 for
an unresolved token and 403 when the role ranks below the route's minimum.

**Alternatives considered.** Plaintext tokens (leak on DB read); offloading to
an external IdP/OIDC (heavy for a self-hosted single binary).

**Consequences / trade-offs.** No recoverable secrets at rest, clean role
ranking, and the resolved actor is threaded into the request context
(`registry.WithActor`) so every mutation is attributable in the audit log
(schema v11). Stored token names are restricted to lowercase letters, digits,
`.`, `_` and `-`, and `root` is reserved, so no token can pass for another
identity in that log. Cost: the env bootstrap token is a single shared admin
credential — powerful, so branchd refuses one shorter than 16 characters.
Rotating it has no side effect on stored data since the at-rest key became
independent of it (ADR-10).

---

## ADR-10: Secrets at rest — AES-256-GCM under a dedicated key

**Context.** With per-branch credential rotation on (ADR-05 / `--rotate-branch-
credentials`), each branch's generated password is persisted in the registry.
Storing those passwords in plaintext in the SQLite file is a leak if the file is
read. The first design derived the key from the admin token
(`sha256(PGOVERLAY_TOKEN)`). That coupled password recoverability to the
credential most likely to be rotated after an incident: rotating the token made
every stored password undecryptable, and because a decrypt failure failed the
whole row read, it took down listing, reconcile, reset and destroy fleet-wide
(issue #9). It also made the registry file an offline oracle for a weak token.

**Decision.** Encrypt branch passwords with **AES-256-GCM**
(`internal/registry/crypto.go`) under a **dedicated random 32-byte key**,
independent of the admin token. branchd takes it from `$PGOVERLAY_SECRET_KEY`,
else `--secret-key-file` (`$PGOVERLAY_SECRET_KEY_FILE`), else
`<state dir>/secret.key`, which it generates (0600, published atomically so HA
replicas agree) on first start. Values are stored as
`enc:v2:<kid>:base64(nonce || ciphertext)`; `kid` is a short domain-separated
hash of the key, so a row names the key it needs. Legacy `enc:v1:` rows
(encrypted under `sha256(PGOVERLAY_TOKEN)`) are read with the current token as a
decrypt-only fallback, and at startup `ReencryptSecrets` moves every live row not
yet under the dedicated key (legacy ciphertext and legacy plaintext) under it.
**A row no configured key can open never fails a read**: the branch comes back
with an empty password and `PasswordUnavailable` set (`password_unavailable` in
the API), so list, routing, reconcile, reset (which mints and stores a new
password) and destroy keep working. A destroyed branch's password is cleared
(schema v15 trigger). Encryption stays optional in the registry package (no key
= plaintext) for tests and embedded use; branchd always configures a key.
Local-mode `pgb` loads the same key (never generates one) plus the legacy token
key, so it can read what branchd wrote.

**Alternatives considered.** Keep deriving the key from the token, with a KDF
and a previous-token list (still couples rotation of two unrelated secrets); an
external KMS (more moving parts for a single-binary tool); no encryption
(plaintext-at-rest leak).

**Consequences / trade-offs.** The admin token rotates freely. A copy of the
registry file alone (a backup, a snapshot, a support bundle) reveals no branch
password. The default key file sits next to the registry, so an attacker who
can read the whole state directory can read both; operators who want the key
elsewhere set `PGOVERLAY_SECRET_KEY` from a secret manager (for example a
Kubernetes Secret). Losing the key does not lose branches, only their stored
passwords, and resetting a branch recovers it. The at-rest key itself rotates
through the key id: retired keys in `PGOVERLAY_SECRET_KEY_PREVIOUS` (and a
state-dir `secret.key` that is no longer the primary) are decrypt-only, and the
startup sweep moves their rows under the new key, after which they can be
dropped.

---

## ADR-11: Copy-on-write strategy — a lazy read-write shim on OverlayFS, clones where the filesystem allows

**Context.** The overlay backend's premise is that a branch pays for what it
changes. The pre-v1 review measured otherwise: PostgreSQL opens every relation
segment `O_RDWR`, even to read it (`src/backend/storage/smgr/md.c`), and
OverlayFS copies a lower file whole on a read-write open, so one
`SELECT count(*)` on a 489 MB table copied the table into the branch
([benchmarks](benchmarks.md#reads-copy-up-too)). Issue
[#49](https://github.com/abd-ulbasit/pgoverlay/issues/49) set the bar for
v1.0.0 on the **default** setup, plain Docker on an ext4 host with stock
`postgres:14` to `18` images: a read-only workload adds approximately
nothing; a write copies as little as the mechanism allows (block level the
target, whole file on first write the minimum); no correctness regressions;
no new privileges; create time independent of database size; Docker Desktop,
Colima, OrbStack and Linux; Kubernetes keeps working. Every option was
researched, prototyped and measured on one host
([the evaluation](benchmarks.md#the-evaluation)). The finding that framed the
decision: nothing gives block-level copy-on-write on a plain ext4 Docker host
with stock images and no new privileges. ext4 cannot reflink, every OverlayFS
variant copies the whole file on a read-write open, and every block-level
mechanism needs a privileged loop, device-mapper or nbd set-up, or a
userspace I/O layer.

**Decision.** Copy on first *write* everywhere, and on first *changed block*
where the filesystem can clone:

- **The lazyrw shim, on by default** on the overlay backend (Docker and
  Kubernetes hostPath). An `LD_PRELOAD` library
  (`internal/cow/lazyrw/lazyrw.c`) opens relation and SLRU files read-only and
  reopens a file read-write, on the same descriptor, at its first write-class
  call. It ships hardened: `truncate(path, 0)` becomes an `O_TRUNC` open (a
  plain truncate copies the whole lower file first); a descriptor is swapped
  only if it is still the file it was; it is active only in the `postgres`
  server binary; every libc pointer has a raw-syscall fallback; a failed
  upgrade fails the write. The entrypoint (`internal/cow/entrypoint.sh`) uses
  it only after a per-start kernel self-test (read-only descriptors must see
  data written after a copy-up: stacked file operations, Linux 4.19) and a
  preload probe, pins `io_method=worker` on PG 18, and otherwise falls back
  to eager copying, reported in `cow-mode`, the log and
  `pgoverlay_branch_cow_mode`. The builds are committed, reproducible and
  embedded (`internal/cow/lazyrw.go`), so `go install` still needs no C
  toolchain. `--lazyrw=off` restores the old behaviour.
- **Seed settle, on by default** (`internal/pgctl/settle.go`). The shim only
  helps if a read writes nothing, and a `pg_basebackup` copy makes reads
  write: backup-label replay, hint bits, pruning, anti-wraparound vacuum.
  Each seed is recovered, `VACUUM (FREEZE, ANALYZE)`d and cleanly shut down
  once (`--seed-settle=freeze|recover|off`), and the WAL segment a branch
  appends to is trimmed to a hole after its shutdown checkpoint, so a
  branch's first WAL write copies about 1 MiB rather than 16 MiB.
- **Clones where the filesystem reflinks, detected, not configured.** On XFS
  (`reflink=1`) and btrfs the kernel already turns copy-up into an extent
  clone. branchd probes the volumes' filesystem at startup
  (`internal/cow/fsprobe.go`), exports `pgoverlay_cow_copyup_mode`, counts
  usage as exclusive bytes with a small FIEMAP tool (`pgoverlay-du`), and on
  XFS sets a 16 KiB copy-on-write extent size hint on a volume root it
  manages. `--volume-root DIR` puts Docker volumes on such a disk without
  moving Docker (`internal/runtime/docker_volumeroot.go`). The shim stays on
  there too: reads then open nothing read-write, so branches keep sharing the
  seed's page cache instead of each cloning its own inode.
- **zfs and csi are unchanged**: their clones were block level already.

**Alternatives considered.**

- *OverlayFS `metacopy=on`, data-only lower layers, `volatile`*: measured
  byte-identical to the baseline; the kernel copies data on any write-mode
  open.
- *fuse-overlayfs*: the same whole-file copy on open, plus a daemon.
- *A managed loopback XFS reflink pool on ext4 hosts* (the only route to
  block-level copy-on-write on ext4): reads copied nothing and writes were
  block level, but `fsync` ran at 0.4 to 0.6 times ext4's rate, TPC-B was
  roughly half (on a noisy host), and it brings a privileged loop device
  lifecycle, re-attach after reboot, and a fixed capacity that fails with
  `ENOSPC` or `EIO`. Deferred to v1.1 as an opt-in, gated on a quiet-host
  benchmark.
- *A per-file `FICLONE` backend without overlay*: would drop `CAP_SYS_ADMIN`
  from branch containers, but needs a reflink filesystem, cannot clone a
  running branch atomically, and its create time grows with the file count.
  A later experiment.
- *btrfs subvolume snapshots, a btrfs pool, dm-thin, dm-snapshot, qcow2 over
  nbd*: block level, but privileged and fragile set-ups, extra kernel modules
  that desktop VMs may lack, and the slowest `fsync` measured.
- *A seccomp user-notification supervisor, a custom FUSE filesystem, a
  userspace block-redirect shim, a hardlink farm*: more moving parts in the
  I/O path than the problem needs.
- *A patched Postgres*: gives up stock images and means maintaining a fork of
  every supported major.
- *`wal_recycle=off` in branches*, to stop a checkpoint copying a seed WAL
  segment up only to rename it: measured, and on a settled seed there is no
  such copy-up left to save (its single segment is already copied by the
  branch's first write), so it only keeps fewer recycled segments at the cost
  of zero-filling each new one. Kept as the experimental `--wal-recycle=off`;
  trimming the segment in the settle is what removed the copy.
- *Keep documenting it and point read-heavy users at zfs or csi*: the v1.0-rc
  answer. It leaves the default backend contradicting its own premise.

**Consequences / trade-offs.** On the default setup reads are free: a read of
a 521 MB frozen table went from +555 MiB and 20.8 s to 0 bytes and 277 ms,
with select-only throughput unchanged and no new privileges. Writes on ext4
still copy a whole segment (up to 1 GiB) on first write: #49's minimum, not
its target. The copy waits in whatever writes the page out, usually a
checkpoint rather than the statement (a one-row `UPDATE` 0.18 s, the next
`CHECKPOINT` 20 s for a 446 MiB segment). The pgbench release gate met its
read half; its write half (warm TPC-B within 5% of the eager branch) was
inconclusive on the shared host it ran on and needs a quiet-host rerun
([benchmarks](benchmarks.md#throughput-and-the-first-write-stall)).
Block level comes only from a reflink filesystem
(automatic, or via `--volume-root`) or, later, the v1.1 pool. Correctness now
depends on the shim seeing every write: a missed write path fails loudly
(`EBADF`, then a Postgres `ERROR`) rather than silently, CI audits the libc
imports of every supported `postgres` binary against the interposition list,
and each new Postgres major has to be re-audited (asynchronous I/O that
writes through `io_uring` would bypass it, hence the `io_method` pin). It
also depends on a kernel property, checked on every start rather than
assumed. The repository now carries prebuilt binaries, kept honest by a
byte-for-byte reproducibility check. Seeding takes longer (a read of the
database plus a write of its unfrozen pages), and a settled seed is analyzed,
which changes how `pgb diff` counts small seeded tables. Branches created
before v1.0.0 stay eager until they are reset. Masking still runs in every
branch and so still copies the segments it rewrites. Only linux/amd64 on one
kernel was measured; arm64 is covered by CI, Docker Desktop, Colima and
OrbStack are expected to work but were not measured.
