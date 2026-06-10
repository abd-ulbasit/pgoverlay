# Vendored CSI test manifests

Manifests for the `PGBRANCH_CSI_IT=1` integration tests (`hack/kind-csi-up.sh`
applies them onto the `pgbranch-test` kind cluster). Vendored — instead of
`kubectl apply`-ing raw.githubusercontent URLs at test time — so the install
is reproducible and pinned.

| Directory | Source | Version |
|---|---|---|
| `snapshotter/` (CRDs + snapshot-controller) | [kubernetes-csi/external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter) `client/config/crd/snapshot.storage.k8s.io_*.yaml`, `deploy/kubernetes/snapshot-controller/` | v8.2.0 |
| `sidecar-rbac/csi-snapshotter-rbac.yaml` | external-snapshotter `deploy/kubernetes/csi-snapshotter/rbac-csi-snapshotter.yaml` | v8.2.0 |
| `sidecar-rbac/external-provisioner-rbac.yaml` | [kubernetes-csi/external-provisioner](https://github.com/kubernetes-csi/external-provisioner) `deploy/kubernetes/rbac.yaml` | v5.2.0 |
| `sidecar-rbac/external-attacher-rbac.yaml` | [kubernetes-csi/external-attacher](https://github.com/kubernetes-csi/external-attacher) `deploy/kubernetes/rbac.yaml` | v4.8.0 |
| `sidecar-rbac/external-resizer-rbac.yaml` | [kubernetes-csi/external-resizer](https://github.com/kubernetes-csi/external-resizer) `deploy/kubernetes/rbac.yaml` | v1.13.1 |
| `sidecar-rbac/external-health-monitor-rbac.yaml` | [kubernetes-csi/external-health-monitor](https://github.com/kubernetes-csi/external-health-monitor) `deploy/kubernetes/external-health-monitor-controller/rbac.yaml` | v0.14.0 |
| `hostpath/` | [kubernetes-csi/csi-driver-host-path](https://github.com/kubernetes-csi/csi-driver-host-path) `deploy/kubernetes-latest/hostpath/` (driverinfo, plugin, snapshotclass; `csi-hostpath-testing.yaml` deliberately omitted) | v1.17.0 |
| `csi-hostpath-storageclass.yaml` | written here (provisioner `hostpath.csi.k8s.io`, `WaitForFirstConsumer`) | — |

The sidecar RBAC versions match the sidecar images referenced by
`hostpath/csi-hostpath-plugin.yaml`, the same pairing
csi-driver-host-path's own `deploy.sh` computes.

To bump: re-copy the files from the new tags and update this table.
