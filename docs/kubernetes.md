# Kubernetes

> Adapted from the [README](https://github.com/abd-ulbasit/pgbranch); the
> README stays the canonical copy of this walkthrough.

branchd can run in-cluster with branches as pods (`--runtime kube`). A Helm
chart deploys the whole thing, in one of two storage modes: **csi** —
branches as PVC clones, schedulable on any node, the recommended
production-ish deployment — or **hostpath**, the single-node/dev mode.

## Recommended: csi mode

When the cluster has a CSI driver that can clone volumes, deploy in csi
mode — branches live in PersistentVolumeClaims (surviving node loss), pods
schedule on any node and need no extra capabilities:

```bash
make docker-build                          # builds pgbranch/branchd:dev (push it, or `kind load` for local clusters)
helm install pgbranch deploy/helm/pgbranch \
  --namespace pgbranch-system --create-namespace \
  --set node=<node-for-branchd-state> \
  --set token=$(openssl rand -hex 16) \
  --set storage.mode=csi \
  --set storage.storageClass=<class-with-clone-support>
```

Requirements:

- A StorageClass whose CSI driver supports **PVC cloning** (PVC
  `dataSource: PersistentVolumeClaim`) — e.g. AWS EBS, Ceph RBD,
  OpenEBS zfs-localpv, LINSTOR — set `storage.storageClass`.
- Alternatively (or additionally) **VolumeSnapshot** support: set
  `storage.snapshotClass` and branches clone via VolumeSnapshot + restore
  instead. The external-snapshotter CRDs + controller must be installed.
- `storage.volumeSize` sizes every pgbranch PVC (default 10Gi; clones are
  thin on CoW drivers).

In csi mode the chart also puts branchd's own state (the sqlite registry)
on a PVC automatically, so the registry is as durable as the branches it
tracks. `persistence.enabled` is a tri-state string: `""` (auto — on with
csi, off with hostpath), `"true"`, `"false"`; `persistence.size` (default
1Gi) and `persistence.storageClass` tune the claim.

How csi mode works (branchd `--kube-storage csi --csi-storage-class …`,
which forces the `csi` CoW backend): the source is seeded into a PVC via
`pg_basebackup`; creating a branch clones that PVC and the branch pod runs
postgres directly on the clone — no overlay, no node pin, no extra
capabilities. Branch-from-branch clones the parent's PVC after a CHECKPOINT
and a brief parent stop (CSI drivers don't guarantee crash-consistent clones
of in-use volumes); the parent pod restarts as soon as the clone is
provisioned, and the wire router re-resolves it transparently. Every clone
is an independent volume: destroying a parent never breaks its children
(resetting a child does need its parent alive, since reset re-clones).

Two csi-mode caveats: branch disk usage (`pgb branch ls` SIZE) reports the
full clone size as the filesystem sees it, not the CoW delta — what a delta
costs depends on the driver. And whether a *live* source PVC can be cloned
while a helper pod holds it is driver-specific; pgbranch only clones source
PVCs with no pod attached (seeding helpers are one-shot), so this does not
come up in normal flows.

## Single-node / dev: hostpath

The default mode needs zero storage infrastructure — all CoW data is plain
directories under `dataRoot` on ONE designated node, and every branch/helper
pod is pinned there (branch pods carry `CAP_SYS_ADMIN` for the in-container
overlay mount). It is the simplest honest setup, and what `make k8s-it`
exercises on kind:

```bash
helm install pgbranch deploy/helm/pgbranch \
  --namespace pgbranch-system --create-namespace \
  --set node=<storage-node-name> \
  --set token=$(openssl rand -hex 16)
```

> **Data-loss warning:** hostpath mode keeps all CoW data *and* the sqlite
> registry on the storage node's disk, and a node rollover (e.g. an EKS
> upgrade rolling the node group) recycles that disk — branches and the
> registry are gone. Branches are disposable by design, so for dev/test the
> recovery is just re-seed and re-branch ([docs/eks.md](eks.md) walks the
> procedure); if branch survival across node loss matters, use csi mode.

## Values that matter

- **`node` (required)** — the name of the node branchd itself runs on
  (`kubectl get nodes`); its sqlite registry lives there in a `hostPath`
  (unless `persistence` puts it on a PVC). In the default `hostpath`
  storage mode it is also the **storage node**: all CoW data lives under
  `dataRoot` (default `/var/lib/pgbranch`) on this one node as plain
  directories, and every branch/helper pod is pinned there.
- **`storage.mode`** — `csi` (recommended, multi-node, branches as PVC
  clones) or `hostpath` (default, single-node/dev — see above).
- **`persistence.*`** — branchd's registry on a PVC (auto-on in csi mode).
- **`token` / `existingSecret`** — the REST API bearer token. Either let the
  chart render a Secret from `token`, or point `existingSecret` at a
  pre-created Secret with key `token`.
- **`proxy.service.type`** — set to `NodePort` (with
  `proxy.service.nodePort`) to reach branches from outside the cluster
  without a port-forward.
- **`rotateBranchCredentials`** — give every branch its own generated
  password instead of inheriting the source's (see
  [architecture](architecture.md)).

## Storage modes: hostpath vs csi

| | `hostpath` (default) | `csi` |
|---|---|---|
| Branch data | directories under `dataRoot` on ONE node | PersistentVolumeClaims |
| Branch creation | empty rw dir + in-container OverlayFS | PVC `dataSource` clone (CoW on capable drivers) |
| Pod placement | every pod pinned to `node` | any node — the scheduler decides |
| Branch pod privileges | `CAP_SYS_ADMIN` (overlay mount) | none |
| Node loss | branches + registry lost (see warning above) | PVCs survive; registry too with `persistence` |
| Storage requirements | none (a disk) | a CSI driver supporting **PVC cloning** or **VolumeSnapshots** |
| Scope | dev/test on one node (kind, a beefy VM) | multi-node clusters, production-ish use |

## What the chart creates

A single-replica Deployment (branchd's registry is SQLite — single writer,
so one replica, `Recreate` strategy, state in `hostPath <dataRoot>/state` on
the storage node — or in a PVC when `persistence` is on, which is automatic
in csi mode), a namespace-scoped Role (pods create/delete/get/list/
watch, pods/exec, pods/log — plus persistentvolumeclaims and volumesnapshots
when `storage.mode=csi`; branchd manages resources only in its own
namespace), and two Services: `pgbranch-api` (REST, :7070) and
`pgbranch-proxy` (Postgres router, :6432). The branchd container runs as
root for write access to its hostPath state dir; in hostpath mode branch
pods get `CAP_SYS_ADMIN` for their in-container overlay mount, same as on
Docker (csi branch pods need nothing).

The chart wires the OverlayFS (hostpath) and csi backends; the experimental
[zfs backend](zfs.md) needs a zpool on the storage node and privileged
helper pods, and is not wired into chart values yet.

## Using it

Same REST API as on Docker; branch hosts are pod IPs, so connect via the
proxy Service:

```bash
kubectl -n pgbranch-system port-forward svc/pgbranch-api 7070 &
curl -H "$AUTH" -d '{"name":"main","host":"db.prod.internal","port":5432,
  "user":"postgres","password":"secret"}' localhost:7070/v1/sources
curl -H "$AUTH" -d '{"name":"pr-42","source":"main"}' localhost:7070/v1/branches

# in-cluster: psql "host=pgbranch-proxy.pgbranch-system port=6432 dbname=postgres@pr-42 user=postgres"
kubectl -n pgbranch-system port-forward svc/pgbranch-proxy 6432 &
psql "host=localhost port=6432 dbname=postgres@pr-42 user=postgres"
```

## Branch per pull request

The chart ships `pgbranch-github` as an optional sub-deployment
(`--set ghook.enabled=true ...`): a signed GitHub webhook creates
`pr-<number>` when a PR opens, optionally resets it on every push, destroys
it on close, and can post a one-time connect-info comment. Setup,
permissions, and the full `GHOOK_*` environment reference live in
[GitHub App](github-app.md).

## Testing

`make helm-test` lints and grep-asserts the rendered chart; `make k8s-it`
runs the full integration suite against a local
[kind](https://kind.sigs.k8s.io) cluster (`hack/kind-up.sh` creates
`pgbranch-test` and preloads images). `make csi-it` exercises the csi mode
end-to-end on the same cluster: `hack/kind-csi-up.sh` installs the
external-snapshotter CRDs/controller and
[csi-driver-host-path](https://github.com/kubernetes-csi/csi-driver-host-path)
(vendored, version-pinned manifests under `hack/csi/`), then the test seeds a
source PVC, clones a branch, verifies isolation over SQL, branches from the
branch, and tears everything down (no PVCs left). A snapshot-mode roundtrip
covers the VolumeSnapshot+restore clone path against
`csi-hostpath-snapclass`.
