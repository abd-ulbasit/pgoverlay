# Kubernetes

branchd can run in-cluster with branches as pods (`--runtime kube`). A Helm
chart deploys the whole thing, in one of two storage modes: **csi** —
branches as PVC clones, schedulable on any node, the recommended
production-ish deployment — or **hostpath**, the single-node/dev mode.

## Recommended: csi mode

When the cluster has a CSI driver that can clone volumes, deploy in csi
mode — branches live in PersistentVolumeClaims (surviving node loss), pods
schedule on any node and need no extra capabilities:

```bash
kubectl create namespace pgoverlay-system
kubectl label namespace pgoverlay-system pod-security.kubernetes.io/enforce=baseline
helm install pgoverlay deploy/helm/pgoverlay \
  --namespace pgoverlay-system \
  --set token=$(openssl rand -hex 16) \
  --set storage.mode=csi \
  --set storage.storageClass=<class-with-clone-support>
```

Nothing needs building first: the chart pulls a published image (see
[Images](#images) below).

Requirements:

- A StorageClass whose CSI driver supports **PVC cloning** (PVC
  `dataSource: PersistentVolumeClaim`) — e.g. AWS EBS, Ceph RBD,
  OpenEBS zfs-localpv, LINSTOR — set `storage.storageClass`.
- Alternatively (or additionally) **VolumeSnapshot** support: set
  `storage.snapshotClass` and branches clone via VolumeSnapshot + restore
  instead. The external-snapshotter CRDs + controller must be installed.
- `storage.volumeSize` sizes every pgoverlay PVC (default 10Gi; clones are
  thin on CoW drivers).

In csi mode the chart also puts branchd's own state (the sqlite registry)
on a PVC automatically, so the registry is as durable as the branches it
tracks. `persistence.enabled` is a tri-state string: `""` (auto — on with
csi, off with hostpath), `"true"`, `"false"`; `persistence.size` (default
1Gi) and `persistence.storageClass` tune the claim. With the registry on a
PVC, branchd is not pinned to any node and `node` is not needed: if its node
goes away, the Deployment brings branchd back wherever its state volume can
attach. (With `persistence.enabled=false` the registry is a hostPath again,
so `node` becomes required and branchd is pinned to it.)

How csi mode works (branchd `--kube-storage csi --csi-storage-class …`,
which forces the `csi` CoW backend): the source is seeded into a PVC via
`pg_basebackup` and settled (see [Seed settle](reference.md#seed-settle));
creating a branch clones that PVC and the branch pod runs postgres directly
on the clone — no overlay, no lazyrw shim, no node pin, no extra
capabilities. What a read or a write costs is the CSI driver's business, as
before. Branch-from-branch clones the parent's PVC after a CHECKPOINT
and a brief parent stop (CSI drivers don't guarantee crash-consistent clones
of in-use volumes); the parent pod restarts as soon as the clone is
provisioned, and the wire router re-resolves it transparently. The same
brief stop happens whenever such a child is **reset or diffed**: its base is
the parent's live PVC, so the reset or diff clones the parent again (and
compares against the parent's *current* state, not the fork point). Every
clone is an independent volume: destroying a parent never breaks its running
children, but once the parent is gone they can no longer be reset or diffed;
`pgb branch reset` and `pgb diff` refuse them up front with a clear error.

Two csi-mode caveats: branch disk usage (`pgb branch ls` SIZE) reports the
full clone size as the filesystem sees it, not the CoW delta — what a delta
costs depends on the driver. And whether a *live* source PVC can be cloned
while a helper pod holds it is driver-specific; pgoverlay only clones source
PVCs with no pod attached (seeding helpers are one-shot), so this does not
come up in normal flows.

## Single-node / dev: hostpath

The default mode needs zero storage infrastructure — all CoW data is plain
directories under `dataRoot` on ONE designated node, and branchd and every
branch/helper pod are pinned there (branch pods carry `CAP_SYS_ADMIN` for the
in-container overlay mount). It is the simplest honest setup, and what
`make k8s-it` exercises on kind:

```bash
kubectl create namespace pgoverlay-system
kubectl label namespace pgoverlay-system pod-security.kubernetes.io/enforce=privileged
helm install pgoverlay deploy/helm/pgoverlay \
  --namespace pgoverlay-system \
  --set node=<storage-node-name> \
  --set token=$(openssl rand -hex 16)
```

> **Data-loss warning:** hostpath mode keeps all CoW data *and* the sqlite
> registry on the storage node's disk, and a node rollover (e.g. an EKS
> upgrade rolling the node group) recycles that disk — branches and the
> registry are gone. Branches are disposable by design, so for dev/test the
> recovery is just re-seed and re-branch ([docs/eks.md](eks.md) walks the
> procedure); if branch survival across node loss matters, use csi mode.

**Copy-on-write in hostpath mode.** Branch pods run the same entrypoint as
Docker branches, so they get the same copy-on-write behaviour:

- **The lazyrw shim** is on by default (`cow.lazyrw: true`, branchd
  `--lazyrw`): reads copy nothing into the branch, and the first write to a
  table file copies that file once. The install helper pod carries the shim
  builds in its environment (in its short-lived Secret, like every helper
  environment) and writes them into the branch's directory; nothing new is
  granted, since branch pods already have `SYS_ADMIN` for the overlay mount.
  Each branch checks the node's kernel (4.19 or later) and its image at
  start, and falls back to copying on open, with a warning in the pod log and
  in `pgoverlay_branch_cow_mode`, when it cannot use the shim.
- **Clone copy-up** comes from the node's filesystem. With `dataRoot`
  (branchd `--kube-data-root`) on XFS with `reflink=1` or on btrfs, a
  branch's first write to a file clones it instead of copying it, and later
  writes copy only the blocks they change. branchd detects this at startup
  (`pgoverlay_cow_copyup_mode`), counts branch usage as the bytes a branch
  does not share, and on XFS sets a 16 KiB copy-on-write extent size hint on
  `dataRoot` (`--xfs-cowextsize`; the chart does not expose it yet). Nodes
  whose root filesystem is XFS with reflink (expected on Amazon Linux 2023,
  the EKS default; branchd's `copy-up probe` log line tells you) get this
  with the default `dataRoot`; elsewhere, mount an XFS or btrfs disk at
  `dataRoot`. See
  [Core concepts](concepts.md#8-clone-or-copy-what-a-copy-up-costs).
- **Seed settle** (`seedSettle: freeze`, branchd `--seed-settle`) runs in a
  helper pod on the branch image after every seed, in both storage modes.

## Images

`image.tag` is empty by default, which means the chart's `appVersion`
(`deploy/helm/pgoverlay/Chart.yaml`) — a tag published to
`ghcr.io/abd-ulbasit/pgoverlay-branchd`. So a plain `helm install` pulls a
real image and there is nothing to build first. `ghook.image.tag` follows the
same appVersion for `ghcr.io/abd-ulbasit/pgoverlay-ghook`. The release
workflow publishes both images for every release tag, with an SBOM and build
provenance, and refuses to release unless `appVersion` equals the tag. From
v1.0.0 they are multi-arch (linux/amd64, linux/arm64), so an arm64 cluster
(kind on Apple Silicon, Graviton nodes) pulls them directly; the earlier `-rc`
images were linux/amd64 only.

To deploy a locally built image instead, build it, get it onto the node, and
say so explicitly:

```bash
make docker-build                                                 # ghcr.io/abd-ulbasit/pgoverlay-branchd:dev
kind load docker-image ghcr.io/abd-ulbasit/pgoverlay-branchd:dev  # or push to a registry the cluster can reach
helm install pgoverlay deploy/helm/pgoverlay ... --set image.tag=dev
```

`dev` is only ever built locally and side-loaded; it is not pushed to any
registry, so it needs both the load/push step and the explicit
`--set image.tag=dev`. `make docker-build-ghook` and `--set ghook.image.tag=dev`
are the ghook equivalents. For a private registry, override
`image.repository`/`ghook.image.repository` too and attach an
`imagePullSecret` ([docs/eks.md](eks.md#images) shows the EKS version).

branchd itself starts two more kinds of image at runtime:

- **postgres:&lt;major&gt;** for branch pods and the seeding helpers, following
  each source's `pg_version`.
- **the utility helper image** for file-level helper pods (seed-volume prep,
  entrypoint installs, disk usage, hostPath volume copies). It defaults to
  alpine **pinned by digest** — the same base the pgoverlay images build on
  (`runtime.UtilityImage`; a unit test keeps the two in step) — because in
  hostpath mode it runs as root with the whole data root mounted. A cluster
  that cannot pull from Docker Hub sets `helperImage` (branchd
  `--kube-helper-image`) to a mirror; pin that by digest too.

## Values that matter

- **`node`** — the node branchd is pinned to, when something it needs lives
  on that node's disk (`kubectl get nodes`). **Required in hostpath mode**,
  where it is also the **storage node**: all CoW data lives under `dataRoot`
  (default `/var/lib/pgoverlay`) on this one node as plain directories, and
  every branch/helper pod is pinned there. Also required whenever the
  registry is a `hostPath` (`persistence.enabled=false`). Not needed, and
  ignored, in csi mode with persistence on.
- **`storage.mode`** — `csi` (recommended, multi-node, branches as PVC
  clones) or `hostpath` (default, single-node/dev — see above).
- **`persistence.*`** — branchd's registry on a PVC (auto-on in csi mode).
- **`nodeSelector` / `tolerations` / `affinity`** — standard scheduling knobs
  for the branchd pod. Unpinned HA replicas (csi with persistence and
  `replicaCount > 1`) get a required `podAffinity` so they share the
  ReadWriteOnce state volume's node; setting `affinity` replaces it.
- **`token` / `existingSecret`** — the REST API bearer token. Either let the
  chart render a Secret from `token`, or point `existingSecret` at a
  pre-created Secret with key `token`.
- **`proxy.service.type`** — set to `NodePort` (with
  `proxy.service.nodePort`) to reach branches from outside the cluster
  without a port-forward. With `LoadBalancer`, restrict who can connect with
  `proxy.service.loadBalancerSourceRanges`, or make the load balancer
  internal with `proxy.service.annotations` (see "Proxy exposure" under
  Pod security & network hardening below).
- **`rotateBranchCredentials`** — give every branch its own generated
  password instead of inheriting the source's (see
  [architecture](architecture.md)).
- **`proxy.tls.certSecret`** — enable wire-protocol TLS on the Postgres
  router (see below). Empty (default) = plaintext: the proxy answers the
  client's `SSLRequest` with `N`.
- **`helperImage`** — the utility helper image override (see
  [Images](#images)).
- **`cow.lazyrw`** — `true` (default) or `false`: whether hostpath branches
  preload the lazyrw shim (branchd `--lazyrw`). Branches pick a change up on
  their next start. csi branches ignore it.
- **`seedSettle`** — `freeze` (default), `recover` or `off`: how a new seed is
  prepared before branches start from it (branchd `--seed-settle`, see
  [Seed settle](reference.md#seed-settle)).
- **`dataRoot`** — where hostpath data lives on the storage node (default
  `/var/lib/pgoverlay`); on XFS (`reflink=1`) or btrfs, copy-up clones (see
  above).
- **`ghook.apiTokenSecret` / `ghook.apiTokenKey`** — the operator-role token
  the webhook service uses (see [Branch per pull request](#branch-per-pull-request)).

## Proxy TLS (cert-manager)

The Postgres router serves TLS when `proxy.tls.certSecret` points at a Secret
holding `tls.crt` and `tls.key`; the chart mounts it into branchd and passes
`--pg-tls-cert`/`--pg-tls-key`, so the proxy answers `SSLRequest` with `S`.

Issue the cert with cert-manager (any `Issuer`/`ClusterIssuer` works — a CA
issuer for in-cluster trust, or an ACME issuer for a publicly reachable LB):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: pgoverlay-proxy-tls
  namespace: pgoverlay-system
spec:
  secretName: pgoverlay-proxy-tls           # <- the Secret the chart consumes
  dnsNames: ["pgoverlay-proxy.example.com"] # how clients address the proxy
  issuerRef:
    name: my-issuer
    kind: ClusterIssuer
```

Then install/upgrade with `--set proxy.tls.certSecret=pgoverlay-proxy-tls`.
cert-manager renews the Secret in place; restart branchd (or rely on its
Recreate strategy on the next chart upgrade) to pick up the rotated cert.
Clients then connect through `dbname@branch` with `sslmode=verify-full` (and
`sslrootcert` pointing at your CA when it is a private one). `sslmode=require`
encrypts but does not check who answered; the default `sslmode=prefer`
silently falls back to plaintext, so do not rely on it.

## Storage modes: hostpath vs csi

| | `hostpath` (default) | `csi` |
|---|---|---|
| Branch data | directories under `dataRoot` on ONE node | PersistentVolumeClaims |
| Branch creation | empty rw dir + in-container OverlayFS | PVC `dataSource` clone (CoW on capable drivers) |
| What a branch copies | nothing on read (lazyrw shim); a file on its first write, or only its changed blocks when `dataRoot` is on XFS or btrfs | whatever the CSI driver's clone copies |
| Pod placement | every pod pinned to `node` | any node — the scheduler decides; branchd follows its state PVC |
| Branch pod privileges | `CAP_SYS_ADMIN` + unconfined seccomp (overlay mount) | none added; RuntimeDefault seccomp |
| Pod Security level | `privileged` | `baseline` (with persistence) |
| Node loss | branches + registry lost (see warning above) | PVCs survive; registry too with `persistence`, and branchd reschedules |
| Storage requirements | none (a disk) | a CSI driver supporting **PVC cloning** or **VolumeSnapshots** |
| Scope | dev/test on one node (kind, a beefy VM) | multi-node clusters, production-ish use |

## What the chart creates

A Deployment for branchd (`replicaCount`, default 1: the registry is SQLite,
a single writer; more than one replica turns on leader election and gives a
standby set on the same node as the state volume, not more throughput, see
[High availability](ha.md)), `Recreate` strategy, state in
`hostPath <dataRoot>/state` on the storage node — or in a PVC when
`persistence` is on, which is automatic in csi mode. A namespace-scoped Role
for branchd, which manages resources only in its own namespace:

- pods create/delete/get/list/watch, pods/exec create, pods/log get;
- secrets create/delete, for the short-lived Secrets that carry helper pods'
  environment (see [Runtime pods](#runtime-pods-branchd-creates)); no
  get/list, branchd never reads Secrets back;
- persistentvolumeclaims and volumesnapshots when `storage.mode=csi`;
- the leader-election rules listed in [High availability](ha.md) when
  `replicaCount > 1` or `leaderElection.enabled`.

And two Services: `pgoverlay-api` (REST, :7070) and `pgoverlay-proxy`
(Postgres router, :6432). With leader election on, `pgoverlay-api` selects
only the leader's pod (`pgoverlay.leader=true`); `pgoverlay-proxy` selects
every replica. The branchd container runs as root for write access to its
hostPath state dir; in hostpath mode branch pods get `CAP_SYS_ADMIN` for their
in-container overlay mount, same as on Docker (csi branch pods need nothing).

**The state volume.** `<dataRoot>/state` (or the persistence PVC) holds the
SQLite registry and `secret.key`, the at-rest key for rotated branch
passwords, which branchd generates (mode `0600`, directory `0700`) on first
start. Back them up together: a registry restored without its key keeps
working, but every rotated password reads as `password_unavailable` until
that branch is reset. The chart does not yet have a value for supplying the
key from a Secret (`PGOVERLAY_SECRET_KEY`).

**Shutdown.** `shutdownTimeout` (default 60 seconds) is how long branchd lets
in-flight operations finish on `SIGTERM` before rolling them back; the chart
sets `terminationGracePeriodSeconds` to `shutdownTimeout + 30` so the kubelet
does not kill it mid-drain.

The chart wires the OverlayFS (hostpath) and csi backends; the experimental
[zfs backend](zfs.md) needs a zpool on the storage node and privileged
helper pods, and is not wired into chart values yet.

## Runtime pods branchd creates

Besides the Deployment, branchd creates two kinds of pod at runtime, in its
own namespace, both with no ServiceAccount token:

- **Branch pods** (`pgoverlay.role=branch`) run each branch's Postgres. They
  are plain Pods, labelled with the owning registry's
  `pgoverlay.instance`.
- **Helper pods** (`pgoverlay.role=helper`, named `pgoverlay-helper-*`) are
  one-shot: seeding (`pg_basebackup` / `pg_dump`) and the seed settle,
  entrypoint and lazyrw shim installs, disk usage, and in hostpath mode
  volume creation, copies and removal, the startup copy-up probe and the XFS
  extent size hint.

A helper's environment — for seeding, the **source database password** — is
never written into the Pod object. branchd puts it in an immutable Secret
with the same name as the pod, created just before the pod and deleted as
soon as the container has started (the kubelet reads environment once, and
helpers never restart), and again when the helper is removed. So nobody
with only `pods get` in the namespace can read it from the pod spec, and
the Secret exists for seconds, not for the whole seed. The one residual copy
is the Secret's create request: if your API server audit policy logs Secrets
at `Request` level or above, the password is in that log (the common and
recommended audit policies log Secrets at `Metadata` only).

Helper pods carry the `pgoverlay.instance` label and, when branchd runs in
the cluster, an ownerReference to branchd's own pod (from
`PGOVERLAY_POD_NAME`/`PGOVERLAY_POD_UID`, which the chart sets). If branchd
is killed mid-seed — a crash, a rollout that outlasts the shutdown budget —
Kubernetes garbage-collects its helpers and their Secrets with its pod
instead of leaving a `pg_basebackup` running against production.

branchd waits for a helper through restarts of its API watch (kube-apiserver
ends every watch after 30-60 minutes; a large seed outlives several), so
long seeds are not cut off. A helper that cannot start fails with the pod's
own explanation: an image-pull or configuration error that persists for 30
seconds, or a pod still not started after 10 minutes (the scheduler's
message, e.g. a volume node-affinity conflict), and a helper deleted from
under branchd fails at once. Branch pods that never become ready likewise
report why — unschedulable, `ImagePullBackOff`, `CrashLoopBackOff` with the
last exit — in the error the branch create fails with, so the reason
survives the pod being cleaned up.

## Pod security & network hardening

**Container securityContext.** Both deployments ship a hardened container
`securityContext` out of the box: `allowPrivilegeEscalation: false`, all
capabilities dropped, `readOnlyRootFilesystem: true` (with a `/tmp` emptyDir
for scratch), and `seccompProfile: RuntimeDefault`. The **ghook** webhook
receiver additionally runs `runAsNonRoot: true` as UID `65532` — it needs no
privileges at all — and mounts no ServiceAccount token. **branchd** keeps
`runAsUser` (default `0`) values-driven because it must write its `hostPath`
state dir, but gets every other control.

**Branch and helper pods vs storage mode.** The pods branchd creates *at
runtime* (not by this chart) are where the privilege story differs:

- **hostpath/overlay mode (default):** branch pods need `CAP_SYS_ADMIN` **and
  an unconfined seccomp/AppArmor profile** to perform the in-container
  OverlayFS mount (a privileged kernel operation), and every pod mounts
  `hostPath` volumes. This is a **single-node / trusted-workload posture** —
  fine for kind/dev/a dedicated box, not for a shared cluster.
- **csi mode (`storage.mode=csi`) — recommended production default:** branches
  are PVC clones, so branch pods get **no added capabilities** and run with
  an explicit `seccompProfile: RuntimeDefault` and
  `allowPrivilegeEscalation: false` on any node (an unset profile would mean
  Unconfined on any kubelet without `seccompDefault`). Prefer csi whenever
  the cluster is shared or untrusted.
- **Helper pods**, in both modes, run with `RuntimeDefault` seccomp and
  `allowPrivilegeEscalation: false`, with two exceptions: in hostpath mode
  the copy-up probe, which mounts an overlay once at branchd startup, runs
  with exactly a branch pod's settings (`SYS_ADMIN`, seccomp and AppArmor
  unconfined); and the experimental zfs backend's helpers are privileged.
- **The lazyrw shim** is an `LD_PRELOAD` library that only the `postgres`
  server process inside a hostpath branch pod loads. It adds no capability
  and no volume; see [Security](security.md#branch-instances).

**Pod Security Admission.** Label the release namespace with the level its
pods need; `helm install` does not check it (Helm does not validate pods), so
a too-strict namespace shows up later as branchd failing to start or as
`pgb branch create` failing with a `violates PodSecurity` error. The chart's
NOTES print the level for your values.

| Configuration | Level | Why |
|---|---|---|
| hostpath mode | `privileged` | hostPath volumes on every pod; `CAP_SYS_ADMIN` + unconfined seccomp on branch pods |
| csi, `persistence.enabled=false` | `privileged` | branchd's registry is a hostPath volume |
| csi with persistence (default) | `baseline` | no hostPath, no added capabilities, RuntimeDefault seccomp |

```bash
kubectl label namespace pgoverlay-system pod-security.kubernetes.io/enforce=baseline   # csi
```

`restricted` fits neither mode: branchd runs as root (for its state dir) and
the postgres entrypoint starts as root to fix data-directory ownership before
dropping to the `postgres` user.

**NetworkPolicies.** Set `networkPolicy.enabled: true` (needs a
policy-enforcing CNI such as Calico or Cilium) to lock traffic down:

- ingress to branchd API/proxy is limited to the ghook pod, the peers in
  `networkPolicy.allowedClients`, and `networkPolicy.metricsFrom` (Prometheus);
- branch pods (selected by `pgoverlay.managed=true,pgoverlay.role=branch`)
  accept connections only from branchd, and may only reach cluster DNS
  (`networkPolicy.dnsFrom`, default kube-dns). Branch pods never talk to the
  Postgres source in any mode — they run on volumes seeded beforehand — so a
  developer with superuser on a branch cannot reach production from it;
- helper pods (`pgoverlay.role=helper`) accept no connections. They are what
  connects to the source when seeding, so `networkPolicy.sourceEgress` (plus
  `networkPolicy.sourcePorts`, default TCP 5432) applies to them: once it is
  set, helpers may reach only cluster DNS and those peers. Left empty, helper
  egress is not restricted (seeding has to reach the source somewhere).

It is **off by default** so existing installs don't lose connectivity, but is
recommended on any multi-tenant cluster. See `values.yaml` for the peer-shape
examples.

**Secret hygiene.** `--set token=` / `--set ghook.webhookSecret=` (and a
values file in git) persist those secrets **in cleartext in the Helm release
history**. For production, pre-create the Secret and reference it with
`existingSecret` / `ghook.existingSecret` so secrets never touch Helm values.

**CI deployer.** `deploy/preview-deployer-rbac.yaml` is a namespaced
ServiceAccount + Role that can `helm upgrade --install` this chart in every
mode (a test renders the chart and checks it). Two things make it broader
than "only the chart's objects": Kubernetes only lets it create branchd's
Role if it holds every permission that Role grants (pods/exec, pods/log,
secrets create/delete, and per mode volumesnapshots and leases), and Helm's
default release storage lists Secrets, which returns their contents — so
that ServiceAccount can read every Secret in its namespace. Keep that
namespace dedicated to pgoverlay; the file's header shows how
`HELM_DRIVER=configmap` narrows it.

**Proxy exposure.** The Postgres proxy speaks the wire protocol in plaintext
unless `proxy.tls.certSecret` is set, and every branch is a copy of
production. Only expose it (NodePort/LB/Ingress) behind TLS or a trusted
boundary; enable `rotateBranchCredentials` so each branch gets its own
password when the proxy is reachable off-cluster; and for a LoadBalancer set
`proxy.service.loadBalancerSourceRanges` to the client CIDRs that need it
(an empty list means the whole internet on most clouds), or make it internal
through `proxy.service.annotations`. `ghook.service` has the same two knobs
for the webhook endpoint.

## Using it

Same REST API as on Docker; branch hosts are pod IPs, so connect via the
proxy Service:

```bash
kubectl -n pgoverlay-system port-forward svc/pgoverlay-api 7070 &
curl -H "$AUTH" -d '{"name":"main","host":"db.prod.internal","port":5432,
  "user":"postgres","password":"secret"}' localhost:7070/v1/sources
curl -H "$AUTH" -d '{"name":"pr-42","source":"main"}' localhost:7070/v1/branches

# in-cluster: psql "host=pgoverlay-proxy.pgoverlay-system port=6432 dbname=postgres@pr-42 user=postgres"
kubectl -n pgoverlay-system port-forward svc/pgoverlay-proxy 6432 &
psql "host=localhost port=6432 dbname=postgres@pr-42 user=postgres"
```

A few things to know when driving it:

- `kubectl port-forward svc/pgoverlay-api` pins one pod for the life of the
  forward. With leader election that is the leader at the time; restart the
  forward after a failover. Clients that go through the Service (in-cluster,
  or through an Ingress or LoadBalancer) follow the leader automatically, and
  the Go client retries the `503`s of a failover.
- `pgb connect` prints the direct pod-IP URL (in-cluster only) and a router
  URL. The chart does not pass `--advertise-proxy-addr` yet, so outside the
  cluster give the router address yourself:
  `pgb connect pr-42 --proxy-host pg.example.com --proxy-port 6432`.
- The chart serves the REST API over plain HTTP inside the cluster. If you
  put TLS in front of it with a private CA, point the CLI at the CA with
  `PGOVERLAY_CA_CERT=<pem file>` (it wins over the insecure
  `PGOVERLAY_TLS_SKIP_VERIFY=1`).
- With `replicaCount > 1`, a query cancel (`Ctrl-C`) only works when the
  cancel request reaches the replica carrying the session. Give the
  `pgoverlay-proxy` Service `sessionAffinity: ClientIP` (the chart does not
  set it yet).

## Branch per pull request

The chart ships `pgoverlay-github` as an optional sub-deployment
(`--set ghook.enabled=true ...`): a signed GitHub webhook creates
`gh-<repo-key>-pr-<number>` when a PR opens (see
[branch names](github-app.md#branch-names)), optionally resets it on every push,
destroys it on close, and keeps one live connect-info comment on the PR
(updated in place as the branch changes) plus a `pgoverlay/branch` commit
status. Setup, permissions, and the full `GHOOK_*` environment reference
live in [GitHub App](github-app.md).

ghook only creates, resets and destroys branches, so give it an
operator-role token rather than branchd's admin token (which also manages
sources, credentials and tokens):

```bash
pgb token create ghook --role operator            # prints the token once
kubectl -n pgoverlay-system create secret generic pgoverlay-ghook-api \
  --from-literal=token=<the printed token>
helm upgrade pgoverlay deploy/helm/pgoverlay -n pgoverlay-system --reuse-values \
  --set ghook.apiTokenSecret=pgoverlay-ghook-api
```

Until `ghook.apiTokenSecret` is set, ghook falls back to the admin token and
the chart's NOTES say so.

## Testing

`make helm-test` lints and grep-asserts the rendered chart (including the
hardening above); `go test ./internal/deploy/` adds offline `helm template`
checks, among them that `deploy/preview-deployer-rbac.yaml` can install the
chart. `make k8s-it` runs the full integration suite against a local
[kind](https://kind.sigs.k8s.io) cluster (`hack/kind-up.sh` creates
`pgoverlay-test` and preloads images). `make csi-it` exercises the csi mode
end-to-end on the same cluster: `hack/kind-csi-up.sh` installs the
external-snapshotter CRDs/controller and
[csi-driver-host-path](https://github.com/kubernetes-csi/csi-driver-host-path)
(vendored, version-pinned manifests under `hack/csi/`), then the test seeds a
source PVC, clones a branch, verifies isolation over SQL, branches from the
branch, and tears everything down (no PVCs left). A snapshot-mode roundtrip
covers the VolumeSnapshot+restore clone path against
`csi-hostpath-snapclass`.
