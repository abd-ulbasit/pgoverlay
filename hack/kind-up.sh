#!/usr/bin/env bash
# Creates the pgbranch-test kind cluster (single node; the control-plane node
# doubles as the pgbranch storage node, data-root /var/lib/pgbranch inside the
# node container) and preloads the images the PGBRANCH_K8S_IT=1 tests need so
# in-cluster pulls don't hit the network. Idempotent.
set -euo pipefail

CLUSTER=pgbranch-test

# Multiple kind clusters exhaust the docker VM's default inotify instance
# limit (128); the kubelet then crashloops with "too many open files". Bump
# the limits in the VM kernel via a privileged container (no-op if already
# high enough).
docker run --privileged --rm alpine:3.21 \
  sysctl -w fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=1048576 >/dev/null

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
fi

for img in postgres:17 alpine:3.21; do
  docker image inspect "$img" >/dev/null 2>&1 || docker pull "$img"
done

# `kind load docker-image` fails with "content digest not found" when the
# docker daemon (containerd image store, e.g. Colima) exports a multi-arch
# manifest list whose other-platform blobs are absent. A single-platform
# archive sidesteps that; fall back to a plain save for older docker.
arch="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
tar="$(mktemp "${TMPDIR:-/tmp}/pgbranch-kind-images.XXXXXX")"
trap 'rm -f "$tar"' EXIT
docker save --platform "$arch" postgres:17 alpine:3.21 -o "$tar" 2>/dev/null ||
  docker save postgres:17 alpine:3.21 -o "$tar"
kind load image-archive "$tar" --name "$CLUSTER"
