# Observability

branchd exposes Prometheus metrics and a real readiness endpoint on the REST
API port. Both sit **outside** the bearer-token auth: scrapers and kubelet
probes don't authenticate, and neither endpoint leaks secrets.

## Endpoints

| Path       | Auth | Purpose                                                                    |
|------------|------|----------------------------------------------------------------------------|
| `/healthz` | none | Liveness — the process is up. Used by the Deployment's `livenessProbe`.     |
| `/readyz`  | none | Readiness — 200 only when the registry is reachable **and** the container driver responds (a cheap `ListManaged`); 503 otherwise. Used by the `readinessProbe`. |
| `/metrics` | none | Prometheus exposition over branchd's private registry.                      |

## Metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `pgbranch_branches_total` | gauge | `state` | Branches by state (reported from the registry on scrape). |
| `pgbranch_sources_total` | gauge | `state` | Sources by state. |
| `pgbranch_branch_op_duration_seconds` | histogram | `op` | Branch operation latency (`op` = create\|reset\|destroy\|from_branch\|diff). |
| `pgbranch_branch_op_errors_total` | counter | `op` | Failed branch operations. |
| `pgbranch_masking_duration_seconds` | histogram | — | Time applying a source's masking scripts inside a branch. |
| `pgbranch_reaper_runs_total` | counter | — | TTL reaper passes. |
| `pgbranch_reaper_reaped_total` | counter | — | Expired branches destroyed by the reaper. |
| `pgbranch_reconcile_runs_total` | counter | — | Reconcile passes. |
| `pgbranch_reconcile_actions_total` | counter | `action` | Reconcile actions (`fail_stuck`\|`remove_orphan_container`\|`gc_layer`\|`gc_volume`). |
| `pgbranch_inflight_ops` | gauge | — | Branch operations currently in flight. |
| `pgbranch_disk_bytes_free` | gauge | — | Free bytes on the **storage-root** filesystem (read via `statfs` on every scrape). |
| `pgbranch_disk_bytes_total` | gauge | — | Total bytes on the storage-root filesystem. |

The disk gauges cover the filesystem that holds **all** branch copy-on-write
volumes **and** the SQLite registry. The storage root is `~/.pgbranch`
(`$PGBRANCH_HOME`) for the docker/overlay backend, or `--kube-data-root`
(default `/var/lib/pgbranch`) on the storage node for `--runtime kube
--kube-storage hostpath`. They are **not** emitted for `--kube-storage csi`,
where each branch gets its own PVC and there is no single shared local root to
measure — watch the CSI driver's own capacity metrics there instead.

## Running out of disk (ENOSPC)

Every branch shares one storage-root filesystem, so a full disk is a
fleet-wide, not per-branch, failure:

- **Overlay copy-up fails.** A write to any branch must copy the touched block
  up into that branch's upper layer; with no free space the copy-up returns
  `ENOSPC` and the write — and often the whole transaction — fails.
- **Postgres write failures across *all* branches.** WAL/heap writes in every
  running branch start failing; branches may refuse to accept writes or shut
  down their backends.
- **Registry corruption risk.** The SQLite registry lives on the same
  filesystem, so a full disk can also break branch bookkeeping (create/destroy
  state transitions), turning a space problem into a control-plane problem.

These surface as confusing Postgres errors with no obvious common cause —
`pgbranch_disk_bytes_free` is the single signal that explains them.

### Recommended alert

Warn well before the disk is full (10% free) so there is time to reap branches
or grow the volume:

```yaml
- alert: PgbranchStorageRootLow
  expr: pgbranch_disk_bytes_free / pgbranch_disk_bytes_total < 0.10
  for: 5m
  labels: { severity: warning }
  annotations:
    summary: "pgbranch storage root <10% free"
    description: >
      The filesystem holding all branch CoW volumes and the registry is
      nearly full. Overlay copy-up and Postgres writes will start failing
      across every branch. Reap branches (TTL/--max-branches) or grow the
      volume.
```

If you prefer an absolute floor (e.g. on a fixed-size PV), alert on
`pgbranch_disk_bytes_free < 5e9` (5 GiB) instead of the ratio.

## Scraping

The Helm chart annotates the branchd pod for Prometheus pod-discovery:

```yaml
metadata:
  annotations:
    prometheus.io/scrape: "true"
    prometheus.io/port: "7070"   # = .Values.api.port
    prometheus.io/path: /metrics
```

If you run a `Prometheus` CRD / `PodMonitor` instead of annotation-based
discovery, point it at the API port and `/metrics`. Outside Kubernetes, scrape
`http://<branchd-host>:7070/metrics` directly.
