# High availability (leader election)

branchd's registry is a single SQLite file on a ReadWriteOnce volume, so it has
exactly one writer. To survive a pod or node failure without losing the control
plane, you can run **more than one replica** and let them elect a leader: the
leader does all the work, the others stand by ready to take over.

By default branchd runs as a single instance with no leader election — this is
the docker/local path and the chart's `replicaCount: 1` default. Nothing below
applies until you opt in.

## How it works

When started with `--leader-elect` (kube runtime only), every branchd replica
contends for a [coordination.k8s.io `Lease`][lease] named **`pgbranch-branchd`**
in its own namespace. Exactly one replica holds the Lease at a time — that's the
leader. The leader's election identity is its pod name (the `POD_NAME` env var,
which the chart wires from `metadata.name`; it falls back to the hostname).

Only the leader:

- runs the **reconcile loop** (TTL reaping, stuck-row failure, orphan-container
  removal, dangling layer/volume GC), and
- accepts **mutating** `/v1` requests (branch/source create, reset, destroy,
  token management, `POST /v1/reconcile`).

Every replica — leader or not — keeps serving `/healthz`, `/readyz`, `/metrics`
and **read-only** `GET /v1/...` requests off its own read-only registry handle
(SQLite reads are safe; the leader is the only writer). A non-leader answers a
mutating request with **`503 not leader`**.

This is an availability/standby setup, **not horizontal scaling**: adding
replicas does not add write throughput — they wait to take over.

## Enabling it

Set either knob in the chart:

```bash
helm upgrade --install pgbranch deploy/helm/pgbranch \
  --set node=<storage-node> \
  --set token=<api-token> \
  --set replicaCount=2          # ⇒ leader election turns on automatically
# or, to keep one replica but still elect (e.g. before scaling up):
#   --set leaderElection.enabled=true
```

When `replicaCount > 1` **or** `leaderElection.enabled=true`, the chart:

- passes `--leader-elect` to branchd,
- sets `POD_NAME` via a `fieldRef` to `metadata.name`, and
- grants the branchd `Role` the `coordination.k8s.io` **`leases`** verbs
  (`get`, `create`, `update`, `watch`, `list`) in the release namespace.

The single-replica default renders none of the above.

## The RWO-PVC co-scheduling caveat

branchd's state (the SQLite registry) lives on a **ReadWriteOnce** volume — a
hostPath on the storage node, or a PVC when `persistence.enabled`. An RWO volume
can only be attached to one node at a time, so **all replicas must schedule onto
that node.** The chart already pins branchd to `node` with `nodeName`, so the
default 2-replica layout co-schedules correctly: two pods on the storage node,
sharing the volume, only the leader writing.

If you want replicas spread across nodes (to survive losing the storage node
itself), put the registry on a **ReadWriteMany** volume — a CSI driver / storage
class that supports RWX — so every replica can mount it from any node. Until
then, HA protects against a pod crash / rollout, not against losing the node the
state lives on.

## Failover behavior

- **Losing leadership** (the leader's Lease renewal fails — pod killed, network
  partition, node pressure): the old leader cancels its reconcile loop and flips
  its mutating gate closed within the Lease's renew deadline, so it stops writing
  promptly.
- **Gaining leadership**: a standby that acquires the Lease opens its mutating
  gate and immediately runs one reconcile pass, converging any drift that
  accumulated during the gap before resuming the normal ticker.
- On a graceful shutdown the leader **releases** the Lease (`ReleaseOnCancel`),
  so a peer takes over without waiting for the full lease to expire.

The default lease timings are a 15s lease duration, a 10s renew deadline and a
2s retry period, so a new leader is typically serving writes within ~15s of the
old one going away.

## Verifying

```bash
# who holds the Lease right now
kubectl -n <ns> get lease pgbranch-branchd -o jsonpath='{.spec.holderIdentity}'

# a follower 503s mutations but still serves reads + probes
curl -s -o /dev/null -w '%{http_code}\n' http://<follower>:7070/healthz   # 200
curl -s -X POST http://<follower>:7070/v1/branches ...                    # 503 not leader
```

[lease]: https://kubernetes.io/docs/concepts/architecture/leases/
