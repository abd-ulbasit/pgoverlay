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

# required values fail fast
if helm template "$CHART" --set token=x >/dev/null 2>&1; then
  echo "FAIL: template without node must fail" >&2; exit 1
fi
if helm template "$CHART" --set node=n >/dev/null 2>&1; then
  echo "FAIL: template without token/existingSecret must fail" >&2; exit 1
fi

echo "helm-test OK"
