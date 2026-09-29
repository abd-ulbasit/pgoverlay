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
| `pgoverlay_branches_total` | gauge | `state` | Branches by state (reported from the registry on scrape). |
| `pgoverlay_sources_total` | gauge | `state` | Sources by state. |
| `pgoverlay_branch_op_duration_seconds` | histogram | `op` | Branch operation latency (`op` = create\|reset\|destroy\|from_branch\|diff). |
| `pgoverlay_branch_op_errors_total` | counter | `op` | Failed branch operations. |
| `pgoverlay_masking_duration_seconds` | histogram | — | Time applying a source's masking scripts inside a branch. |
| `pgoverlay_reaper_runs_total` | counter | — | Reconcile passes that applied a plan (the TTL-reaping half; counted once per apply). |
| `pgoverlay_reaper_reaped_total` | counter | — | Expired branches destroyed by reconcile (`reap` actions are counted here, not below). |
| `pgoverlay_reconcile_runs_total` | counter | — | Reconcile passes. |
| `pgoverlay_reconcile_actions_total` | counter | `action` | Reconcile actions taken (`fail_stuck`\|`fail_stuck_source`\|`retry_destroy`\|`restart_branch`\|`update_endpoint`\|`remove_orphan_container`\|`remove_orphan_helper`\|`gc_layer`\|`gc_volume`); see [what each does](troubleshooting.md#what-reconcile-does). |
| `pgoverlay_compensation_failures_total` | counter | `kind` | Saga compensations or failure transitions that themselves failed (`kind` = `transition`\|`undo`\|`cleanup`). Each one may have left a resource behind for reconcile to collect. |
| `pgoverlay_inflight_ops` | gauge | — | Branch operations currently in flight. |
| `pgoverlay_leader` | gauge | — | `1` on the replica that is leader (accepts mutations, runs reconcile), `0` on followers; always `1` without `--leader-elect`. |
| `pgoverlay_leader_transitions_total` | counter | — | Times this replica gained or lost leadership. |
| `pgoverlay_disk_bytes_free` | gauge | — | Free bytes on the measured filesystem (read via `statfs` on every scrape; see below). |
| `pgoverlay_disk_bytes_total` | gauge | — | Total bytes on the measured filesystem. |
| `pgoverlay_branch_cow_mode` | gauge | `mode` | Ready overlay branches by the copy-on-write mode their Postgres started in: `lazyrw` (the shim is active: reads copy nothing, a file is copied into the branch on its first write), `eager` (it is not, although `--lazyrw=on`; see [Troubleshooting](troubleshooting.md#a-branch-copies-eagerly)), `off` (`--lazyrw=off`) or `unknown` (not read yet or unreadable). Overlay backend only. |

**What the disk gauges measure.** They `statfs` one path on every scrape, and
that path is not always where branch data lives:

| Setup | Path measured by default | Is branch data there? |
|---|---|---|
| Docker runtime | `PGOVERLAY_HOME` (`~/.pgoverlay`) | **No.** Branch and seed volumes are Docker volumes under the engine's data root (`/var/lib/docker/volumes`, inside the VM on Colima or Docker Desktop, or on another machine for a remote engine). The gauges cover the registry's filesystem only, unless both happen to be the same filesystem |
| Kubernetes hostpath, chart layout (`PGOVERLAY_HOME` inside `--kube-data-root`) | the mounted state directory, `<dataRoot>/state` | **Yes** when the state directory is the hostPath (`persistence` off, the default in hostpath mode), since it sits on the data root's filesystem. With `persistence.enabled=true` it is the registry PVC instead |
| Kubernetes hostpath, branchd running on the storage node itself | `--kube-data-root` | **Yes** |
| Kubernetes csi | not emitted | Each branch is its own PVC; watch the CSI driver's capacity metrics |

`--disk-root <path>` overrides the choice: point it at a path on the
filesystem that holds the branch data (for Docker, the engine's data root
when branchd runs on the Docker host; for hostpath with persistence, a mount
of the data root). branchd logs the path it measures at startup
(`disk gauges measure the filesystem of ...`).

## Alerts worth having

```yaml
- alert: PgoverlayCompensationFailures   # leaked resources to look for
  expr: increase(pgoverlay_compensation_failures_total[15m]) > 0
  labels: { severity: warning }
- alert: PgoverlayNoLeader               # every write is refused
  expr: max(pgoverlay_leader) == 0
  for: 1m
  labels: { severity: critical }
- alert: PgoverlayLeaderFlapping
  expr: increase(pgoverlay_leader_transitions_total[15m]) > 4
  labels: { severity: warning }
- alert: PgoverlayBranchOpsFailing
  expr: increase(pgoverlay_branch_op_errors_total[15m]) > 0
  labels: { severity: info }
- alert: PgoverlayBranchesCopyEagerly   # reads copy whole tables into branches
  expr: pgoverlay_branch_cow_mode{mode="eager"} > 0
  for: 15m
  labels: { severity: warning }
```

A compensation failure means a saga's cleanup did not complete; the next
reconcile passes usually collect what it left, and branchd's log names the
resource. The leader alerts matter only with `--leader-elect` (see
[High availability](ha.md#monitoring)).

## Running out of disk (ENOSPC)

On the overlay backend every branch shares one filesystem (the Docker data
root, or the storage node's data root), so a full disk is a fleet-wide, not
per-branch, failure. A write copies the whole file it touches (a table
segment, up to 1 GiB) into the branch the first time. Branches in eager mode
(`pgoverlay_branch_cow_mode{mode="eager"}` or `off`) grow on **reads** too:
there the first time Postgres opens a table file, OverlayFS copies it whole
into the branch ([Reads copy up too](benchmarks.md#reads-copy-up-too)), so a
test suite that scans large tables fills the disk faster than its writes
suggest.

- **Overlay copy-up fails.** The first write to a file in any branch (the
  first open, in eager mode, even for a read) must copy the whole file up
  into that branch's upper layer; with no free space the copy-up returns
  `ENOSPC` and the query — and often the whole transaction — fails.
- **Postgres write failures across *all* branches.** WAL/heap writes in every
  running branch start failing; branches may refuse to accept writes or shut
  down their backends.
- **Registry failures.** When the SQLite registry lives on the same
  filesystem (Kubernetes hostpath without persistence), a full disk can also
  break branch bookkeeping (create/destroy state transitions), turning a space
  problem into a control-plane problem.

These surface as confusing Postgres errors with no obvious common cause.
`pgoverlay_disk_bytes_free` explains them when it measures the right
filesystem (see the table above); on Docker, watch the engine host's disk
directly, or run branchd on that host with `--disk-root` pointing at the
Docker data root. `pgb branch ls --usage` shows which branches hold the
space.

### Recommended alert

Warn well before the disk is full (10% free) so there is time to reap branches
or grow the volume:

```yaml
- alert: PgoverlayStorageRootLow
  expr: pgoverlay_disk_bytes_free / pgoverlay_disk_bytes_total < 0.10
  for: 5m
  labels: { severity: warning }
  annotations:
    summary: "pgoverlay storage root <10% free"
    description: >
      The filesystem branchd measures (the branch data root, when
      configured as described in docs/observability.md) is nearly full.
      Overlay copy-up and Postgres writes will start failing across every
      branch. Reap branches (TTL/--max-branches) or grow the volume.
```

If you prefer an absolute floor (e.g. on a fixed-size PV), alert on
`pgoverlay_disk_bytes_free < 5e9` (5 GiB) instead of the ratio.

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
