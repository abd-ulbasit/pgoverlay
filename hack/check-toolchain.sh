#!/usr/bin/env bash
# Asserts the Dockerfiles' golang base image matches go.mod's `go` directive.
#
# The official golang images set GOTOOLCHAIN=local, so they will not fetch a
# newer toolchain on demand: a base image older than the `go` directive fails
# at `RUN go mod download` with
#
#   go: go.mod requires go >= X (running Y; GOTOOLCHAIN=local)
#
# which breaks `make docker-build` — the first command in docs/kubernetes.md —
# and every test that builds an image (internal/deploy's helm ITs).
set -euo pipefail
cd "$(dirname "$0")/.."

want=$(awk '/^go [0-9]/ {print $2; exit}' go.mod)
[ -n "$want" ] || { echo "FAIL: no 'go' directive in go.mod" >&2; exit 1; }

rc=0
for df in Dockerfile Dockerfile.ghook; do
  got=$(sed -n 's/^FROM golang:\([0-9][^-@ ]*\).*/\1/p' "$df" | head -1)
  if [ -z "$got" ]; then
    echo "FAIL: $df has no 'FROM golang:<version>' line" >&2
    rc=1
  elif [ "$got" != "$want" ]; then
    echo "FAIL: $df builds on golang:$got but go.mod requires go $want" >&2
    echo "      (the golang image pins GOTOOLCHAIN=local, so it cannot upgrade itself)" >&2
    rc=1
  else
    echo "ok: $df golang:$got == go.mod go $want"
  fi
done
exit $rc
