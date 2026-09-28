.PHONY: build test it k8s-it csi-it matrix lint vuln vuln-test check-toolchain docker-build docker-build-ghook helm-test js-sdk-test release-check

# Build identity stamped into every binary (`pgb version`, `branchd -version`,
# `pgoverlay-github -version`). Override for release builds, e.g.
# `make build VERSION=v1.0.0`. Outside a git checkout it falls back to "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG := github.com/abd-ulbasit/pgoverlay/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT) -X $(VERSION_PKG).Date=$(DATE)

build:
	go build -ldflags "$(LDFLAGS)" -o bin/pgb ./cmd/pgb
	go build -ldflags "$(LDFLAGS)" -o bin/branchd ./cmd/branchd
	go build -ldflags "$(LDFLAGS)" -o bin/pgoverlay-github ./cmd/pgoverlay-github

test:
	go test ./...

# The integration targets run one test package at a time (-p 1), as CI does.
# The Docker suites publish ephemeral host ports on one daemon, and the
# Kubernetes suites (internal/runtime, internal/deploy) create and share the
# same pgoverlay-test kind cluster (the HA suite kills pods in it); run in
# parallel they race each other.
it:
	PGOVERLAY_IT=1 go test ./... -count=1 -p 1 -timeout 25m

k8s-it:
	PGOVERLAY_K8S_IT=1 go test ./... -count=1 -p 1 -timeout 30m

# CSI storage mode e2e on kind: hack/kind-csi-up.sh installs the
# external-snapshotter + csi-driver-host-path stack (vendored, pinned
# manifests under hack/csi/), then seed -> PVC-clone branch -> verify ->
# branch-from-branch -> destroy.
csi-it:
	PGOVERLAY_CSI_IT=1 go test ./internal/runtime/ -run TestKubeCSI -count=1 -v -timeout 40m

# Postgres version matrix: seed -> branch -> verify -> destroy per major
# (default "14 18"; override with PGOVERLAY_MATRIX_VERSIONS="14 15 16 17 18").
# Pulls one postgres:<major> image per version.
matrix:
	PGOVERLAY_MATRIX_IT=1 go test ./internal/engine/ -run Matrix -count=1 -v -timeout 25m

lint:
	go vet ./...

# The CI supply-chain gate, verbatim: govulncheck (pinned in the script) in
# binary mode against the three shipped binaries, with the per-advisory
# allowlist in hack/vuln-allowlist.txt (rationale in SECURITY.md). CI runs this
# same script, so a local pass means a CI pass. Needs jq and network access
# (it installs the pinned govulncheck and reads vuln.go.dev).
vuln:
	hack/vulncheck.sh

# Offline tests for the gate itself: a stub scanner drives hack/vulncheck.sh
# through its pass, fail and fail-closed paths.
vuln-test:
	hack/vulncheck_test.sh

# Asserts the Dockerfiles' golang base image matches go.mod's `go` directive.
# The golang image pins GOTOOLCHAIN=local, so drift here breaks every image
# build (and the helm ITs that build one) at `go mod download`.
check-toolchain:
	hack/check-toolchain.sh

docker-build:
	docker build -t ghcr.io/abd-ulbasit/pgoverlay-branchd:dev .

docker-build-ghook:
	docker build -f Dockerfile.ghook -t ghcr.io/abd-ulbasit/pgoverlay-ghook:dev .

helm-test:
	hack/helm-test.sh

# Both JS SDKs: sdk/js (npm package pgoverlay-test) and sdk/js-connect
# (pgoverlay-connect). Needs Node 18+; override with
# `make js-sdk-test NODE=/path/to/node NPM=/path/to/npm`.
NODE ?= node
NPM ?= npm
js-sdk-test:
	cd sdk/js && $(NODE) --test test/*.test.mjs
	cd sdk/js && $(NPM) pack --dry-run
	cd sdk/js-connect && $(NODE) --test test/*.test.mjs
	cd sdk/js-connect && $(NPM) pack --dry-run

# Validates .goreleaser.yaml with the goreleaser version the release workflow
# pins. `make release-check GORELEASER_ARGS="release --snapshot --clean"`
# builds every release archive locally into dist/ without publishing.
GORELEASER_VERSION ?= v2.18.2
GORELEASER_ARGS ?= check
release-check:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) $(GORELEASER_ARGS)
