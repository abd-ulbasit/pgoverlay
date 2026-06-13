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
