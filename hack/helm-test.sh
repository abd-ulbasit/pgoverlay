#!/usr/bin/env bash
# Lints the pgoverlay chart and grep-asserts the critical fields in rendered
# templates (default-ish and custom values), without a cluster (make helm-test).
set -euo pipefail
cd "$(dirname "$0")/.."
CHART=deploy/helm/pgoverlay

has() { grep -qF -- "$2" <<<"$1" || { echo "FAIL: missing '$2' in $3 render" >&2; exit 1; }; }
hasnt() { ! grep -qF -- "$2" <<<"$1" || { echo "FAIL: unexpected '$2' in $3 render" >&2; exit 1; }; }

# The chart's default image tag is Chart.yaml's appVersion: values.yaml pins
# image.tag to "" and both deployments render
# `{{ .Values.image.tag | default .Chart.AppVersion }}`. appVersion is
# therefore the literal tag kubelet pulls, and it has to be one that was
# actually pushed to GHCR. `dev` is NOT: it is only ever built locally by
# `make docker-build` and side-loaded, and defaulting to it is what put a
# straight-from-the-README `helm install` into ImagePullBackOff.
APPVERSION=$(awk -F': *' '/^appVersion:/{gsub(/"/,"",$2); print $2; exit}' "$CHART/Chart.yaml")
[ -n "$APPVERSION" ] || { echo "FAIL: no appVersion in $CHART/Chart.yaml" >&2; exit 1; }

helm lint "$CHART" --set node=test-node --set token=t >/dev/null

# default values (only the two required ones set)
out=$(helm template pgoverlay "$CHART" --set node=storage-1 --set token=s3cret)
has "$out" '--runtime=kube' default
has "$out" '--kube-node=storage-1' default
has "$out" '--kube-data-root=/var/lib/pgoverlay' default
has "$out" 'nodeName: storage-1' default
has "$out" 'path: /var/lib/pgoverlay/state' default
has "$out" 'value: /var/lib/pgoverlay/state' default # PGOVERLAY_HOME
has "$out" 'name: pgoverlay-token' default           # secretKeyRef + rendered Secret
has "$out" 'kind: Secret' default
has "$out" 'pods/exec' default
has "$out" "ghcr.io/abd-ulbasit/pgoverlay-branchd:$APPVERSION" default
hasnt "$out" 'pgoverlay-branchd:dev' default # `dev` was never pushed anywhere
# The chart deploys only branchd; SYS_ADMIN belongs to the branch pods branchd
# creates at runtime and must NOT leak into any chart-rendered manifest.
hasnt "$out" 'SYS_ADMIN' default

# The local-build path stays available: --set image.tag=dev must still reach
# the image `make docker-build` produces and side-loads (README, docs).
out=$(helm template pgoverlay "$CHART" --set node=n --set token=t --set image.tag=dev)
has "$out" 'ghcr.io/abd-ulbasit/pgoverlay-branchd:dev' local-tag
hasnt "$out" "pgoverlay-branchd:$APPVERSION" local-tag

# custom values: existing secret, custom data root, NodePort proxy
out=$(helm template rel "$CHART" --set node=worker-9 --set existingSecret=my-token \
  --set dataRoot=/mnt/pgoverlay --set proxy.service.type=NodePort --set proxy.service.nodePort=30432)
has "$out" '--kube-node=worker-9' custom
has "$out" '--kube-data-root=/mnt/pgoverlay' custom
has "$out" 'nodeName: worker-9' custom
has "$out" 'path: /mnt/pgoverlay/state' custom
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
has "$out" 'claimName: rel-pgoverlay-state' csi-persistence
hasnt "$out" 'path: /var/lib/pgoverlay/state' csi-persistence
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone \
  --set persistence.enabled=false)
hasnt "$out" 'kind: PersistentVolumeClaim' csi-no-persistence
has "$out" 'path: /var/lib/pgoverlay/state' csi-no-persistence
out=$(helm template pgoverlay "$CHART" --set node=storage-1 --set token=s3cret)
hasnt "$out" 'kind: PersistentVolumeClaim' default

# csi without the optional values renders no empty flags
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone)
has "$out" '--kube-storage=csi' csi-minimal
hasnt "$out" '--csi-snapshot-class' csi-minimal
hasnt "$out" '--csi-volume-size' csi-minimal

# default (hostpath) renders no csi args and no PVC RBAC
out=$(helm template pgoverlay "$CHART" --set node=storage-1 --set token=s3cret)
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
out=$(helm template pgoverlay "$CHART" --set node=storage-1 --set token=s3cret)
hasnt "$out" 'ghook' default
hasnt "$out" 'GHOOK_' default

# ghook enabled: deployment + service + secret, wired to the api service
out=$(helm template rel "$CHART" --set node=worker-9 --set token=s3cret \
  --set ghook.enabled=true --set ghook.webhookSecret=whsec --set ghook.source=main \
  --set ghook.githubToken=ghp_abc --set ghook.repos='acme/widgets' \
  --set ghook.proxyHost=pg.example.com:30432 --set ghook.resetOnPush=true)
has "$out" "ghcr.io/abd-ulbasit/pgoverlay-ghook:$APPVERSION" ghook # follows appVersion, as branchd does
hasnt "$out" 'pgoverlay-ghook:dev' ghook
has "$out" 'name: rel-pgoverlay-ghook' ghook # deployment/service/secret share the name
has "$out" 'GHOOK_PGOVERLAY_SERVER' ghook
has "$out" 'value: http://rel-pgoverlay-api:7070' ghook # in-cluster DNS to branchd
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

# --- kube hardening (docs/kubernetes.md, "Pod security & network hardening") ---

# branchd creates and deletes the short-lived Secrets that carry helper env
# (the seed password), and never reads Secrets back; it learns its own pod so
# helpers can be owned by it; the helper image is overridable.
out=$(helm template pgoverlay "$CHART" --set node=storage-1 --set token=s3cret)
has "$out" 'resources: ["secrets"]' default
has "$out" 'verbs: ["create", "delete"]' default
has "$out" 'name: PGOVERLAY_POD_NAME' default
has "$out" 'name: PGOVERLAY_POD_UID' default
has "$out" 'fieldPath: metadata.uid' default
hasnt "$out" '--kube-helper-image' default
out=$(helm template pgoverlay "$CHART" --set node=storage-1 --set token=s3cret \
  --set helperImage=registry.internal/alpine@sha256:abc)
has "$out" '--kube-helper-image=registry.internal/alpine@sha256:abc' helper-image

# csi with its state on a PVC pins branchd to no node (so it comes back after
# the node is replaced) and needs no `node` at all; without persistence the
# state is a hostPath again and the pin (and `node`) are back.
out=$(helm template rel "$CHART" --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone)
hasnt "$out" 'nodeName:' csi-unpinned
hasnt "$out" '--kube-node' csi-unpinned
out=$(helm template rel "$CHART" --set node=worker-3 --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone)
hasnt "$out" 'nodeName:' csi-unpinned-node-set
out=$(helm template rel "$CHART" --set node=worker-3 --set token=t \
  --set storage.mode=csi --set storage.storageClass=fast-clone --set persistence.enabled=false)
has "$out" 'nodeName: worker-3' csi-hostpath-state
if helm template "$CHART" --set token=t --set storage.mode=csi --set storage.storageClass=fast-clone \
  --set persistence.enabled=false >/dev/null 2>&1; then
  echo "FAIL: csi with a hostPath state dir and no node must fail" >&2; exit 1
fi
# unpinned HA replicas share the RWO state PVC, so they must co-locate
out=$(helm template rel "$CHART" --set token=t --set replicaCount=2 \
  --set storage.mode=csi --set storage.storageClass=fast-clone)
has "$out" 'podAffinity:' csi-ha
has "$out" 'topologyKey: kubernetes.io/hostname' csi-ha
out=$(helm template rel "$CHART" --set node=n --set token=t --set replicaCount=2)
hasnt "$out" 'podAffinity' hostpath-ha # nodeName already co-locates them
out=$(helm template rel "$CHART" --set node=n --set token=t --set nodeSelector.pool=pg \
  --set 'tolerations[0].key=dedicated' --set 'tolerations[0].operator=Exists')
has "$out" 'pool: pg' scheduling
has "$out" 'key: dedicated' scheduling

# ghook: no ServiceAccount token in the internet-facing pod, and an operator
# token when one is provided instead of branchd's admin token
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set ghook.enabled=true --set ghook.webhookSecret=w --set ghook.source=main)
has "$out" 'automountServiceAccountToken: false' ghook
ghook_token=$(awk '/name: GHOOK_PGOVERLAY_TOKEN/{f=1} f&&/key:/{print; exit} f&&/name: rel-/{print}' <<<"$out")
has "$ghook_token" 'name: rel-pgoverlay-token' ghook-admin-fallback
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set ghook.enabled=true --set ghook.webhookSecret=w --set ghook.source=main \
  --set ghook.apiTokenSecret=ghook-api --set ghook.apiTokenKey=tok)
ghook_token=$(awk '/name: GHOOK_PGOVERLAY_TOKEN/{f=1} f&&/name: ghook-api/{print} f&&/key:/{print; exit}' <<<"$out")
has "$ghook_token" 'name: ghook-api' ghook-operator-token
has "$ghook_token" 'key: tok' ghook-operator-token

# load balancer exposure can be restricted; the field is only valid (and only
# rendered) for type LoadBalancer
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set proxy.service.type=LoadBalancer --set 'proxy.service.loadBalancerSourceRanges={203.0.113.0/24}' \
  --set proxy.service.annotations.team=db \
  --set ghook.enabled=true --set ghook.webhookSecret=w --set ghook.source=main \
  --set ghook.service.type=LoadBalancer --set 'ghook.service.loadBalancerSourceRanges={192.30.252.0/22}')
has "$out" '- 203.0.113.0/24' proxy-lb
has "$out" 'team: db' proxy-lb
has "$out" '- 192.30.252.0/22' ghook-lb
out=$(helm template rel "$CHART" --set node=n --set token=t \
  --set 'proxy.service.loadBalancerSourceRanges={203.0.113.0/24}')
hasnt "$out" 'loadBalancerSourceRanges' proxy-clusterip

# NetworkPolicy: branch pods never reach the source (DNS-only egress); the
# seed helper pods get sourceEgress, and only them
out=$(helm template rel "$CHART" --set node=n --set token=t --set networkPolicy.enabled=true \
  --set 'networkPolicy.sourceEgress[0].ipBlock.cidr=10.0.5.10/32')
branch_np=$(awk '/name: rel-pgoverlay-branch-pods/{f=1} f&&/^---/{exit} f' <<<"$out")
helper_np=$(awk '/name: rel-pgoverlay-helper-pods/{f=1} f&&/^---/{exit} f' <<<"$out")
has "$branch_np" 'k8s-app: kube-dns' netpol-branch
hasnt "$branch_np" '10.0.5.10' netpol-branch
has "$helper_np" 'pgoverlay.role: helper' netpol-helper
has "$helper_np" 'cidr: 10.0.5.10/32' netpol-helper
has "$helper_np" '- Egress' netpol-helper
out=$(helm template rel "$CHART" --set node=n --set token=t --set networkPolicy.enabled=true)
helper_np=$(awk '/name: rel-pgoverlay-helper-pods/{f=1} f&&/^---/{exit} f' <<<"$out")
has "$helper_np" '- Ingress' netpol-helper-no-source
hasnt "$helper_np" 'Egress' netpol-helper-no-source # seeding must still reach the source

echo "helm-test OK"
