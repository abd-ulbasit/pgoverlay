#!/usr/bin/env bash
# Asserts that what the repository builds with its own Go toolchain can be
# built with it:
#
# 1. The Dockerfiles' golang base image matches go.mod's `go` directive. The
#    official golang images set GOTOOLCHAIN=local, so they will not fetch a
#    newer toolchain on demand: a base image older than the `go` directive
#    fails at `RUN go mod download` with
#
#      go: go.mod requires go >= X (running Y; GOTOOLCHAIN=local)
#
#    which breaks `make docker-build` — the first command in
#    docs/kubernetes.md — and every test that builds an image
#    (internal/deploy's helm ITs).
#
# 2. The goreleaser `make release-check` builds with `go run` asks for no newer
#    Go than go.mod does: under GOTOOLCHAIN=local a goreleaser whose go.mod
#    needs a newer Go fails the same way before it starts. The release
#    workflow must pin the same version. This check reads the pinned
#    version's go.mod through the module proxy, so it needs network access.
set -euo pipefail
cd "$(dirname "$0")/.."

want=$(awk '/^go [0-9]/ {print $2; exit}' go.mod)
[ -n "$want" ] || { echo "FAIL: no 'go' directive in go.mod" >&2; exit 1; }

rc=0
for df in Dockerfile Dockerfile.ghook; do
  # The optional --platform flag is how the build stage cross-compiles from
  # the builder's platform for multi-arch images.
  got=$(sed -nE 's/^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?golang:([0-9][^-@ ]*).*/\2/p' "$df" | head -1)
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

# newer A B: Go version A is strictly newer than B.
newer() { [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -1)" = "$1" ]; }

gr=$(sed -nE 's/^GORELEASER_VERSION[[:space:]]*\?=[[:space:]]*(v[^[:space:]]+).*/\1/p' Makefile | head -1)
gr_wf=$(sed -nE 's/^[[:space:]]+version:[[:space:]]*(v[^[:space:]]+).*GORELEASER_VERSION.*/\1/p' .github/workflows/release.yml | head -1)
if [ -z "$gr" ] || [ -z "$gr_wf" ]; then
  echo "FAIL: goreleaser pin not found (Makefile GORELEASER_VERSION: '$gr'; release.yml version: '$gr_wf')" >&2
  rc=1
elif [ "$gr" != "$gr_wf" ]; then
  echo "FAIL: the Makefile pins goreleaser $gr but .github/workflows/release.yml pins $gr_wf" >&2
  rc=1
else
  gomod=$(go list -m -f '{{.GoMod}}' "github.com/goreleaser/goreleaser/v2@$gr")
  gr_go=$(awk '/^go [0-9]/ {print $2; exit}' "$gomod")
  if [ -z "$gr_go" ]; then
    echo "FAIL: goreleaser $gr: no 'go' directive in $gomod" >&2
    rc=1
  elif newer "$gr_go" "$want"; then
    echo "FAIL: goreleaser $gr requires go >= $gr_go, newer than go.mod's go $want," >&2
    echo "      so 'make release-check' cannot build it under GOTOOLCHAIN=local;" >&2
    echo "      pin a goreleaser whose go.mod needs go $want or older" >&2
    rc=1
  else
    echo "ok: goreleaser $gr (Makefile, release.yml) needs go $gr_go <= go.mod go $want"
  fi
fi
exit $rc
