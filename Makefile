.PHONY: build test it k8s-it csi-it matrix lint vuln check-toolchain docker-build docker-build-ghook helm-test js-sdk-test

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

it:
	PGOVERLAY_IT=1 go test ./... -count=1 -timeout 25m

k8s-it:
	PGOVERLAY_K8S_IT=1 go test ./... -count=1 -timeout 30m

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

# The CI supply-chain gate, verbatim: govulncheck in binary mode against both
# shipped binaries, with the module-scoped allowlist documented in SECURITY.md.
# CI runs this same script, so a local pass means a CI pass. Needs jq;
# installs govulncheck if it is not already on PATH.
vuln:
	hack/vulncheck.sh

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

# JS test-suite SDK (sdk/js, npm package pgoverlay-test). Needs Node 18+;
# override with `make js-sdk-test NODE=/path/to/node NPM=/path/to/npm`.
NODE ?= node
NPM ?= npm
js-sdk-test:
	cd sdk/js && $(NODE) --test test/*.test.mjs
	cd sdk/js && $(NPM) pack --dry-run
