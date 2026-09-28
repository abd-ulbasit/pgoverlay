# branchd container image (what the Helm chart runs). pgb stays a host CLI.
# Pure-Go build (modernc.org/sqlite): CGO off, static binary, tiny runtime image.
# Base images pinned by digest (resolved from Docker Hub; supply-chain hygiene).
# Refresh with: docker buildx imagetools inspect golang:1.26.6-alpine (and alpine:3.24).
#
# Multi-arch: the build stage always runs on the builder's own platform and
# cross-compiles for TARGETOS/TARGETARCH, so `docker buildx build --platform
# linux/amd64,linux/arm64` needs no emulation. A plain `docker build` sets both
# to the daemon's platform.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
# Stamped by the release workflow; `dev` for local builds.
ARG VERSION=dev COMMIT="" DATE=""
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w \
        -X github.com/abd-ulbasit/pgoverlay/internal/version.Version=${VERSION} \
        -X github.com/abd-ulbasit/pgoverlay/internal/version.Commit=${COMMIT} \
        -X github.com/abd-ulbasit/pgoverlay/internal/version.Date=${DATE}" \
      -o /branchd ./cmd/branchd

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# image.source is what makes GHCR link the package to this repo (and show the
# README on the package page). Without it a pushed package is an orphan with no
# provenance trail back to the source.
LABEL org.opencontainers.image.source="https://github.com/abd-ulbasit/pgoverlay" \
      org.opencontainers.image.description="pgoverlay branchd — copy-on-write Postgres branches" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /branchd /usr/local/bin/branchd
# NOTE: the runtime image keeps the default root user because, in hostPath
# mode, branchd writes its registry to a state directory on the node's
# filesystem (a hostPath volume) that is owned by root. branchd itself mounts
# nothing: the overlay mounts happen inside the branch pods it creates. The
# Helm chart pins runAsUser (values.yaml) and applies the rest of the hardening
# (no privilege escalation, all capabilities dropped, RuntimeDefault seccomp,
# read-only root filesystem); hack/helm-test.sh asserts those fields.
ENTRYPOINT ["branchd"]
