#!/usr/bin/env bash
# Installs the CSI test stack onto the pgbranch-test kind cluster (creating it
# via kind-up.sh first): external-snapshotter CRDs + snapshot-controller and
# csi-driver-host-path (supports PVC clones and VolumeSnapshots), plus the
# csi-hostpath-sc StorageClass and csi-hostpath-snapclass VolumeSnapshotClass
# the PGBRANCH_CSI_IT=1 tests use. All manifests are vendored and pinned under
# hack/csi/ (see hack/csi/README.md). Idempotent.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=pgbranch-test
KCTL=(kubectl --context "kind-$CLUSTER")

hack/kind-up.sh

echo "installing external-snapshotter CRDs + snapshot-controller (v8.2.0)"
"${KCTL[@]}" apply -f hack/csi/snapshotter/snapshot.storage.k8s.io_volumesnapshotclasses.yaml \
  -f hack/csi/snapshotter/snapshot.storage.k8s.io_volumesnapshotcontents.yaml \
  -f hack/csi/snapshotter/snapshot.storage.k8s.io_volumesnapshots.yaml
"${KCTL[@]}" apply -f hack/csi/snapshotter/rbac-snapshot-controller.yaml \
  -f hack/csi/snapshotter/setup-snapshot-controller.yaml

echo "installing csi-driver-host-path (v1.17.0) + sidecar RBAC"
"${KCTL[@]}" apply -f hack/csi/sidecar-rbac/
"${KCTL[@]}" apply -f hack/csi/hostpath/
"${KCTL[@]}" apply -f hack/csi/csi-hostpath-storageclass.yaml

echo "waiting for the snapshot controller and hostpath plugin to be ready"
"${KCTL[@]}" -n kube-system rollout status deployment/snapshot-controller --timeout=180s
"${KCTL[@]}" rollout status statefulset/csi-hostpathplugin --timeout=300s

echo "kind-csi-up OK"
