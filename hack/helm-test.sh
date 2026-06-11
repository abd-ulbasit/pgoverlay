#!/usr/bin/env bash
# Lints the pgbranch chart and grep-asserts the critical fields in rendered
# templates (default-ish and custom values), without a cluster (make helm-test).
set -euo pipefail
cd "$(dirname "$0")/.."
CHART=deploy/helm/pgbranch

has() { grep -qF -- "$2" <<<"$1" || { echo "FAIL: missing '$2' in $3 render" >&2; exit 1; }; }
hasnt() { ! grep -qF -- "$2" <<<"$1" || { echo "FAIL: unexpected '$2' in $3 render" >&2; exit 1; }; }

helm lint "$CHART" --set node=test-node --set token=t >/dev/null

# default values (only the two required ones set)
out=$(helm template pgbranch "$CHART" --set node=storage-1 --set token=s3cret)
has "$out" '--runtime=kube' default
has "$out" '--kube-node=storage-1' default
has "$out" '--kube-data-root=/var/lib/pgbranch' default
has "$out" 'nodeName: storage-1' default
has "$out" 'path: /var/lib/pgbranch/state' default
has "$out" 'value: /var/lib/pgbranch/state' default # PGBRANCH_HOME
has "$out" 'name: pgbranch-token' default           # secretKeyRef + rendered Secret
has "$out" 'kind: Secret' default
has "$out" 'pods/exec' default
# The chart deploys only branchd; SYS_ADMIN belongs to the branch pods branchd
# creates at runtime and must NOT leak into any chart-rendered manifest.
hasnt "$out" 'SYS_ADMIN' default

# custom values: existing secret, custom data root, NodePort proxy
out=$(helm template rel "$CHART" --set node=worker-9 --set existingSecret=my-token \
  --set dataRoot=/mnt/pgbranch --set proxy.service.type=NodePort --set proxy.service.nodePort=30432)
has "$out" '--kube-node=worker-9' custom
has "$out" '--kube-data-root=/mnt/pgbranch' custom
has "$out" 'nodeName: worker-9' custom
has "$out" 'path: /mnt/pgbranch/state' custom
has "$out" 'name: my-token' custom
hasnt "$out" 'kind: Secret' custom # existingSecret suppresses the chart Secret
has "$out" 'type: NodePort' custom
has "$out" 'nodePort: 30432' custom
hasnt "$out" 'SYS_ADMIN' custom

# csi storage mode: --kube-storage/--csi-* args + PVC/snapshot RBAC; still no
# SYS_ADMIN anywhere (csi branch pods need no capabilities at all)
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone \
  --set storage.snapshotClass=fast-snap --set storage.volumeSize=20Gi)
has "$out" '--kube-storage=csi' csi
has "$out" '--csi-storage-class=fast-clone' csi
has "$out" '--csi-snapshot-class=fast-snap' csi
has "$out" '--csi-volume-size=20Gi' csi
has "$out" 'persistentvolumeclaims' csi
has "$out" 'volumesnapshots' csi
has "$out" 'snapshot.storage.k8s.io' csi
hasnt "$out" 'SYS_ADMIN' csi

# registry-on-a-PVC: auto-on with csi (state PVC + claim, no hostPath state),
# explicitly off stays off, hostpath default stays hostPath
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone)
has "$out" 'kind: PersistentVolumeClaim' csi-persistence
has "$out" 'claimName: rel-pgbranch-state' csi-persistence
hasnt "$out" 'path: /var/lib/pgbranch/state' csi-persistence
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone \
  --set persistence.enabled=false)
hasnt "$out" 'kind: PersistentVolumeClaim' csi-no-persistence
has "$out" 'path: /var/lib/pgbranch/state' csi-no-persistence
out=$(helm template pgbranch "$CHART" --set node=storage-1 --set token=s3cret)
hasnt "$out" 'kind: PersistentVolumeClaim' default

# csi without the optional values renders no empty flags
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone)
has "$out" '--kube-storage=csi' csi-minimal
hasnt "$out" '--csi-snapshot-class' csi-minimal
hasnt "$out" '--csi-volume-size' csi-minimal

# default (hostpath) renders no csi args and no PVC RBAC
out=$(helm template pgbranch "$CHART" --set node=storage-1 --set token=s3cret)
hasnt "$out" '--kube-storage' default
hasnt "$out" '--csi-storage-class' default
hasnt "$out" 'persistentvolumeclaims' default

# csi mode requires a storage class; unknown modes fail fast
if helm template "$CHART" --set node=n --set token=t --set storage.mode=csi >/dev/null 2>&1; then
  echo "FAIL: storage.mode=csi without storage.storageClass must fail" >&2; exit 1
fi
if helm template "$CHART" --set node=n --set token=t --set storage.mode=nfs >/dev/null 2>&1; then
  echo "FAIL: unknown storage.mode must fail" >&2; exit 1
fi

# ghook is off by default: no webhook resources in the default render
out=$(helm template pgbranch "$CHART" --set node=storage-1 --set token=s3cret)
hasnt "$out" 'ghook' default
hasnt "$out" 'GHOOK_' default

# ghook enabled: deployment + service + secret, wired to the api service
out=$(helm template rel "$CHART" --set node=worker-9 --set token=s3cret \
  --set ghook.enabled=true --set ghook.webhookSecret=whsec --set ghook.source=main \
  --set ghook.githubToken=ghp_abc --set ghook.repos='acme/widgets' \
  --set ghook.proxyHost=pg.example.com:30432 --set ghook.resetOnPush=true)
has "$out" 'pgbranch/ghook:dev' ghook
has "$out" 'name: rel-pgbranch-ghook' ghook # deployment/service/secret share the name
has "$out" 'GHOOK_PGBRANCH_SERVER' ghook
has "$out" 'value: http://rel-pgbranch-api:7070' ghook # in-cluster DNS to branchd
has "$out" 'GHOOK_WEBHOOK_SECRET' ghook
has "$out" 'key: webhook-secret' ghook
has "$out" 'key: github-token' ghook
has "$out" 'webhook-secret: "whsec"' ghook
has "$out" 'GHOOK_SOURCE' ghook
has "$out" 'GHOOK_RESET_ON_PUSH' ghook
has "$out" 'GHOOK_REPOS' ghook
has "$out" 'GHOOK_PROXY_HOST' ghook
has "$out" 'GHOOK_TTL' ghook
hasnt "$out" 'SYS_ADMIN' ghook

# ghook existingSecret suppresses the rendered ghook Secret
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set ghook.enabled=true --set ghook.existingSecret=my-ghook --set ghook.source=main)
has "$out" 'name: my-ghook' ghook-existing
hasnt "$out" 'webhook-secret: ' ghook-existing # no ghook Secret stringData rendered

# ghook required values fail fast when enabled
if helm template "$CHART" --set node=n --set token=t --set ghook.enabled=true \
  --set ghook.source=main >/dev/null 2>&1; then
  echo "FAIL: ghook without webhookSecret/existingSecret must fail" >&2; exit 1
fi
if helm template "$CHART" --set node=n --set token=t --set ghook.enabled=true \
  --set ghook.webhookSecret=w >/dev/null 2>&1; then
  echo "FAIL: ghook without source must fail" >&2; exit 1
fi

# required values fail fast
if helm template "$CHART" --set token=x >/dev/null 2>&1; then
  echo "FAIL: template without node must fail" >&2; exit 1
fi
if helm template "$CHART" --set node=n >/dev/null 2>&1; then
  echo "FAIL: template without token/existingSecret must fail" >&2; exit 1
fi

echo "helm-test OK"
