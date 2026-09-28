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
contends for a [coordination.k8s.io `Lease`][lease] named **`pgoverlay-branchd`**
in its own namespace. Exactly one replica holds the Lease at a time — that's the
leader. The leader's election identity is its pod name (the `POD_NAME` env var,
which the chart wires from `metadata.name`; it falls back to the hostname).

Only the leader:

- runs the **reconcile loop** (TTL reaping, stuck-row failure, orphan-container
  removal, dangling layer/volume GC), and
- accepts **mutating** `/v1` requests: branch/source create, reset, destroy,
  source refresh, masking scripts, token management, `POST /v1/reconcile`, and
  `GET /v1/branches/{name}/diff` (a diff provisions a throwaway instance and
  writes a registry row, so it is routed like a mutation).

A follower keeps serving `/healthz`, `/readyz`, `/metrics` and read-only
`GET /v1/...` requests, and answers a mutating request with **`503 not leader`**
(after the usual token and role checks, so a caller without a valid token gets
`401`/`403` and learns nothing about leadership). Every replica opens the same
read-write registry; what keeps followers from writing is this gate, not a
read-only handle.

This is an availability/standby setup, **not horizontal scaling**: adding
replicas does not add write throughput — they wait to take over.

## How clients reach the leader

The leader labels its own pod **`pgoverlay.leader=true`** for as long as it
leads (and removes the label from any other pod, e.g. a previous leader that
lost the Lease without reaching the apiserver). With leader election on, the
chart's API Service (`<release>-api`) selects that label in addition to the
usual selector labels, so:

- everything that goes through the Service — the CLI with `--server`, SDKs,
  the GitHub Action, the chart's ghook deployment — reaches the leader, reads
  included;
- followers stay **Ready** (Deployment rollouts, `helm install --wait` and the
  Deployment's `Available` condition are unaffected) but receive no API
  traffic through the Service;
- `kubectl port-forward svc/<release>-api 7070` forwards to the leader. A
  port-forward pins one pod for its lifetime, so after a failover restart it.

Talking to a pod directly (pod IP, `port-forward pod/...`) bypasses this: a
follower answers mutations with `503 not leader`. During a failover there is
briefly no labelled pod (the Service has no endpoints) and a request can also
hit the old leader as it steps down; clients should retry `503` and connection
errors with backoff for about a lease duration. The Go client that `pgb` and
the webhook service use does this for you, within a bound: it retries `503`
for any request, `502`, `504` and connection resets for idempotent ones, and
dial failures, with jittered backoff over about eight seconds, closing idle
connections between tries so the Service can pick a new endpoint. A failover
that takes longer than that (a crashed leader's Lease runs 15 s) still
surfaces as an error; retry the command.

The Postgres proxy Service (`<release>-proxy`) still selects every replica:
the wire-protocol router only reads the registry, so any replica can serve it.
One exception: a query cancel request (`Ctrl-C` in psql) is a separate
connection, and only the replica that carries the session knows where to
forward it. With several replicas behind the Service, set
`sessionAffinity: ClientIP` on `<release>-proxy` so a client's cancel reaches
the same replica (the chart does not set it yet); otherwise a cancel may be
dropped.

### Why `/readyz` does not depend on leadership

There were two other ways to keep mutations off followers: fail `/readyz` on
followers, or have followers forward mutations to the leader. Neither was
chosen.

- **A not-ready follower breaks rollouts.** The chart uses the `Recreate`
  strategy, so every replica has to be Ready: `helm install --wait`,
  `kubectl rollout status` and the Deployment's `Available` condition would
  all stay stuck while one pod is a follower.
- **It would also take followers out of the proxy Service.** Readiness
  applies to the whole pod. The API and the Postgres proxy share one
  container, so a not-ready follower would stop serving Postgres traffic too.
- **Forwarding adds a hop that can fail.** Each follower would have to find
  the leader's address and proxy authenticated, long-running requests (diffs
  and seeds take minutes) to it. That is another place for timeouts and
  errors, and during a failover the leader it forwards to may already be gone.

So `/readyz` answers one question: can this process serve (registry
reachable, driver responding)? Leadership is published separately, as the
`pgoverlay.leader` label, and only the API Service selects on it. `/healthz`
stays the liveness probe.

## Enabling it

Set either knob in the chart:

```bash
helm upgrade --install pgoverlay deploy/helm/pgoverlay \
  --set node=<storage-node> \
  --set token=<api-token> \
  --set replicaCount=2          # ⇒ leader election turns on automatically
# or, to keep one replica but still elect (e.g. before scaling up):
#   --set leaderElection.enabled=true
```

When `replicaCount > 1` **or** `leaderElection.enabled=true`, the chart:

- passes `--leader-elect` to branchd,
- sets `POD_NAME` via a `fieldRef` to `metadata.name`,
- grants the branchd `Role` the `coordination.k8s.io` **`leases`** verbs
  (`get`, `create`, `update`, `watch`, `list`) and **`patch`** on `pods` (for
  the leader label) in the release namespace, and
- adds `pgoverlay.leader: "true"` to the API Service's selector.

RBAC cannot narrow `patch` to the release's own pods (their names are
generated), so the Role can patch any pod in the namespace. That is the same
scope as the pod `create`/`delete` branchd already has, and branchd only ever
changes the `pgoverlay.leader` label.

The single-replica default renders none of the above.

Running branchd with `--leader-elect` outside the chart: set `POD_NAME` to the
pod's name to get the leader label (without it branchd logs that it is not
labelling, and you have to route API traffic to the Lease holder yourself),
and select `pgoverlay.leader=true` in whatever Service fronts the API.

## The RWO-PVC co-scheduling caveat

branchd's state (the SQLite registry and the at-rest key) lives on a
**ReadWriteOnce** volume — a hostPath on the storage node, or a PVC when
`persistence` is on (the default in csi mode). An RWO volume can only be
attached to one node at a time, so **all replicas must schedule onto that
node.** The chart handles both layouts:

- **hostpath mode, or `persistence.enabled=false`**: branchd is pinned to
  `node` with `nodeName`, so every replica lands on the storage node.
- **csi mode with persistence**: branchd is not pinned and `node` is not
  needed. With more than one replica the chart adds a required pod affinity
  (replicas co-locate on one node, wherever the scheduler puts the first), so
  they share the PVC's node. Setting `affinity` replaces that rule; keep an
  equivalent one.

If you want replicas spread across nodes (to survive losing the storage node
itself), put the registry on a **ReadWriteMany** volume — a CSI driver / storage
class that supports RWX — so every replica can mount it from any node. Until
then, HA protects against a pod crash / rollout, not against losing the node the
state lives on.

## Failover behavior

- **Losing leadership** (the leader's Lease renewal fails — network
  partition, apiserver trouble, node pressure): within the Lease's renew
  deadline the old leader closes its mutating gate, cancels its reconcile loop
  and **cancels every mutation it still has in flight**. Those sagas run their
  compensations (a half-created branch is rolled back and marked `failed`) and
  their callers get a `503` saying leadership moved, which they can retry
  against the new leader. It then removes its leader label. It does not keep
  writing next to the new leader.
- **Gaining leadership**: a standby that acquires the Lease opens its mutating
  gate, labels its pod (the API Service switches to it) and immediately runs
  one reconcile pass, converging any drift that accumulated during the gap
  before resuming the normal ticker.
- **Graceful shutdown** (rollout, `kubectl delete pod`): branchd stops
  accepting connections and new mutations and gives in-flight requests up to
  `--shutdown-timeout` (chart value `shutdownTimeout`, default 60s) to finish;
  sagas still running after that are cancelled and rolled back. Only then does
  the leader **release** the Lease (`ReleaseOnCancel`), so a peer takes over
  without waiting for the Lease to expire, but never while the old leader is
  still finishing work. The chart sets `terminationGracePeriodSeconds` to
  `shutdownTimeout + 30` so the kubelet does not kill branchd mid-drain.
- **Crash** (no graceful shutdown): the Lease expires after its duration and a
  standby takes over; rows the crashed leader left in `creating`/`resetting`
  are failed by the reconcile loop after `--stuck-timeout`.

The default lease timings are a 15s lease duration, a 10s renew deadline and a
2s retry period, so a new leader is typically serving writes within ~15s of the
old one dying (sooner after a graceful shutdown).

## Monitoring

Every replica exports `pgoverlay_leader` (1 on the leader, 0 on followers; 1
without `--leader-elect`) and `pgoverlay_leader_transitions_total`. Useful
alerts:

```promql
# nobody can take the Lease (e.g. leases RBAC missing): every write is refused
max(pgoverlay_leader) == 0          # for: 1m
# leadership flapping
increase(pgoverlay_leader_transitions_total[15m]) > 4
```

Each replica also logs a warning once a minute while no replica holds a live
Lease or it cannot read the Lease at all.

## Verifying

```bash
# who holds the Lease right now, and which pod carries the leader label
kubectl -n <ns> get lease pgoverlay-branchd -o jsonpath='{.spec.holderIdentity}'
kubectl -n <ns> get pods -l pgoverlay.leader=true

# the API Service's only endpoint is the leader
kubectl -n <ns> get endpointslices -l kubernetes.io/service-name=<release>-api \
  -o jsonpath='{.items[*].endpoints[*].targetRef.name}'

# a follower serves probes and reads, and 503s authorized mutations
curl -s -o /dev/null -w '%{http_code}\n' http://<follower>:7070/healthz   # 200
curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://<follower>:7070/v1/branches -d '{"name":"x","source":"main"}'    # 503 not leader
```

[lease]: https://kubernetes.io/docs/concepts/architecture/leases/
