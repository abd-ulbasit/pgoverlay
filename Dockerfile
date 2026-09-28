# branchd container image (what the Helm chart runs). pgb stays a host CLI.
# Pure-Go build (modernc.org/sqlite): CGO off, static binary, tiny runtime image.
# Base images pinned by digest (resolved from Docker Hub; supply-chain hygiene).
# Refresh with: docker buildx imagetools inspect golang:1.26.6-alpine (and alpine:3.24).
FROM golang:1.26.6-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /branchd ./cmd/branchd

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# image.source is what makes GHCR link the package to this repo (and show the
# README on the package page). Without it a pushed package is an orphan with no
# provenance trail back to the source.
LABEL org.opencontainers.image.source="https://github.com/abd-ulbasit/pgoverlay" \
      org.opencontainers.image.description="pgoverlay branchd — copy-on-write Postgres branches" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /branchd /usr/local/bin/branchd
# NOTE: branchd needs root in hostPath/overlay mode (writes the hostPath state
# dir and overlay-mounts), so the runtime image keeps the default root user.
# The Helm chart pins runAsUser (values.yaml) and applies the rest of the
# hardening (no privilege escalation, dropped caps, RuntimeDefault seccomp).
ENTRYPOINT ["branchd"]
