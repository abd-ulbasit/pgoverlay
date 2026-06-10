# Kubernetes

> Adapted from the [README](https://github.com/abd-ulbasit/pgbranch); the
> README stays the canonical copy of this walkthrough.

branchd can run in-cluster with branches as pods (`--runtime kube`). A Helm
chart deploys the whole thing:

```bash
make docker-build                          # builds pgbranch/branchd:dev (push it, or `kind load` for local clusters)
helm install pgbranch deploy/helm/pgbranch \
  --namespace pgbranch-system --create-namespace \
  --set node=<storage-node-name> \
  --set token=$(openssl rand -hex 16)
```

## Values that matter

- **`node` (required)** — the name of the **storage node** (`kubectl get
  nodes`). All CoW data lives under `dataRoot` (default `/var/lib/pgbranch`)
  on this one node as plain directories; branchd, every branch pod, and every
  helper pod are pinned there with `nodeName` + `hostPath`. This is the
  honest dev/test scope — one node's disk, no CSI; multi-node storage is
  future work.
- **`token` / `existingSecret`** — the REST API bearer token. Either let the
  chart render a Secret from `token`, or point `existingSecret` at a
  pre-created Secret with key `token`.
- **`proxy.service.type`** — set to `NodePort` (with
  `proxy.service.nodePort`) to reach branches from outside the cluster
  without a port-forward.

## What the chart creates

A single-replica Deployment (branchd's registry is SQLite — single writer,
so one replica, `Recreate` strategy, state in `hostPath <dataRoot>/state` on
the storage node), a namespace-scoped Role (pods create/delete/get/list/
watch, pods/exec, pods/log — branchd manages pods only in its own
namespace), and two Services: `pgbranch-api` (REST, :7070) and
`pgbranch-proxy` (Postgres router, :6432). The branchd container runs as
root for write access to its hostPath state dir; branch pods get
`CAP_SYS_ADMIN` for their in-container overlay mount, same as on Docker.

The chart deploys the OverlayFS backend only; the experimental
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
`pgbranch-test` and preloads images).
