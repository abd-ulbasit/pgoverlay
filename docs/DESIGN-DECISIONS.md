# Design decisions (ADRs)

Ten decisions that shaped pgbranch, each recorded in the same four parts:

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

**Context.** pgbranch provisions stateful resources (volumes, Postgres
containers) and needs a reconcile/GC loop to converge reality with its
registry — exactly the workload the operator pattern was built for. The pull to
"just write a controller with a `Branch` CRD" is strong. But the engine must
also run on a laptop under Docker, with no apiserver in sight.

**Decision.** Ship `branchd`, a single daemon (`cmd/branchd/main.go`) exposing a
REST control plane (`--api-addr`, default `:7070`) and a Postgres router
(`--pg-addr`), driven by a CLI and a Helm chart (`deploy/helm/pgbranch`). There
are no CustomResourceDefinitions anywhere in the tree (`grep
CustomResourceDefinition` over the repo returns nothing). The reconciliation
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
`foreign_keys(1)` pragmas, and `db.SetMaxOpenConns(1)` serializes all writers
(registry.go:115–119). Schema is versioned by `PRAGMA user_version`: a
`migrations` slice where entry *i* upgrades version *i*→*i+1*, applied in a
transaction that bumps the pragma (`internal/registry/schema.go`, now through
**v11** — actor column on the audit log).

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

**Consequences / trade-offs.** One stable endpoint, auth-transparent, branch
selection in the connection string (works with any Postgres client). Cost: the
proxy is an **unauthenticated routing surface** — anyone who can dial it can
attempt to route. Hardening in code: uniform `genericRouteRefusal` so an
unauth client can't enumerate branch names or distinguish "unknown" from
"not-ready" from "down"; a startup-phase deadline, a `MaxConns` connection cap
(fast-refuse, not queue), and an idle timeout against slow-loris/DoS. Production
posture leans further on TLS, NetworkPolicy and credential rotation.

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
`coordination.k8s.io` Lease named `pgbranch-branchd`
(`internal/ha/leader.go`, client-go `leaderelection`). Only the leader runs the
reconcile loop and accepts **mutating** `/v1` requests; the API composes a
`LeaderGate` in front of every mutating route (`internal/api/leader.go` —
`mutate = requireLeader ∘ requireRole`), returning **503 "not leader"** on
followers. Reads, `/healthz`, `/readyz`, `/metrics` and the proxy serve from any
replica off a read-only registry handle. Gaining the Lease opens the gate and
runs an immediate reconcile to converge drift; losing it cancels the loop and
closes the gate. The gate defaults to `leader=true`, so with election **off**
(Docker / single instance) every node is always leader and mutations behave
normally.

**Alternatives considered.** A distributed multi-writer store (defeats ADR-02);
active/active without coordination (registry corruption).

**Consequences / trade-offs.** Availability without giving up single-writer
safety; reads/proxy scale out. Cost: writes are not HA-scaled (only the leader
mutates), and an RWO PVC binds all replicas to one node (a co-scheduling caveat
called out in the Helm values).

---

## ADR-08: Instance-scoped reconcile and GC

**Context.** The reconcile loop reaps orphaned containers and volumes. On a
shared Docker daemon — or in the parallel integration-test suite — several
pgbranch instances coexist. A naive "reap anything managed" would have one
instance destroy another's *live* resources.

**Decision.** Every managed resource is stamped with the owning registry's
instance id under the label `pgbranch.instance`
(`runtime.LabelInstance`). `instanceLabels` in `internal/engine/saga.go` is the
single chokepoint that adds it, so no call site can forget. The instance id is a
stable value minted on first registry open and stored in the `meta` table
(schema v8). Reconcile filters strictly on it: `PlanReconcile`
(`internal/engine/reconcile.go`) skips any managed container whose
`pgbranch.instance` label is absent or names another instance, and
`ListManagedVolumes(ctx, instanceID)` scopes volume GC the same way.

**Alternatives considered.** A dedicated daemon per host (operationally
heavier); reaping by `pgbranch.managed=true` alone (cross-instance data loss).

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
env var `PGBRANCH_TOKEN` is the **admin bootstrap**: it is never stored, and is
matched in the middleware with a **constant-time compare**
(`crypto/subtle.ConstantTimeCompare`, `internal/api/middleware.go`,
`resolveActor`) under the `root` audit sentinel. `requireRole` returns 401 for
an unresolved token and 403 when the role ranks below the route's minimum.

**Alternatives considered.** Plaintext tokens (leak on DB read); offloading to
an external IdP/OIDC (heavy for a self-hosted single binary).

**Consequences / trade-offs.** No recoverable secrets at rest, clean role
ranking, and the resolved actor is threaded into the request context
(`registry.WithActor`) so every mutation is attributable in the audit log
(schema v11). Cost: the env bootstrap token is a single shared admin
credential — powerful, and rotating it has a side effect (ADR-10).

---

## ADR-10: Secrets at rest — AES-256-GCM, key derived from the admin token

**Context.** With per-branch credential rotation on (ADR-05 / `--rotate-branch-
credentials`), each branch's generated password is persisted in the registry.
Storing those passwords in plaintext in the SQLite file is a leak if the file is
read.

**Decision.** Encrypt branch passwords at rest with **AES-256-GCM**
(`internal/registry/crypto.go`, `secretBox`): a random 12-byte nonce is
prepended to the ciphertext, base64-encoded, and tagged with an `enc:v1:` prefix
(the prefix versions the scheme so a future KDF/AEAD can coexist). The key is
`sha256(PGBRANCH_TOKEN)` (`DeriveSecretKey`). Encryption is **optional**: a nil
box (no token) stores plaintext, and values without the `enc:` prefix are read
back as legacy plaintext — back-compat for inherit-mode and pre-encryption rows.

**Alternatives considered.** A separate KMS/keyfile (more moving parts for a
single-binary tool); no encryption (plaintext-at-rest leak).

**Consequences / trade-offs.** Branch passwords are unreadable from the raw DB
file without the token, with no new dependency. Cost: because the key is derived
from `PGBRANCH_TOKEN`, **rotating the token orphans every existing encrypted
password** (decrypt fails with the wrong key). This is deliberate and acceptable
for ephemeral branches — re-run rotation (reset the branch) after a token change
to re-encrypt under the new key; `decrypt` returns a loud, actionable error
rather than leaking ciphertext, and it is documented in `docs/usage.md`.
