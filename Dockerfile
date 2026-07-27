# branchd container image (what the Helm chart runs). pgb stays a host CLI.
# Pure-Go build (modernc.org/sqlite): CGO off, static binary, tiny runtime image.
# Base images pinned by digest (resolved from Docker Hub; supply-chain hygiene).
# Refresh with: docker buildx imagetools inspect golang:1.26.5-alpine (and alpine:3.21).
FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /branchd ./cmd/branchd

FROM alpine:3.21@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d
COPY --from=build /branchd /usr/local/bin/branchd
# NOTE: branchd needs root in hostPath/overlay mode (writes the hostPath state
# dir and overlay-mounts), so the runtime image keeps the default root user.
# The Helm chart pins runAsUser (values.yaml) and applies the rest of the
# hardening (no privilege escalation, dropped caps, RuntimeDefault seccomp).
ENTRYPOINT ["branchd"]
