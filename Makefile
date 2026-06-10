.PHONY: build test it k8s-it matrix lint docker-build docker-build-ghook helm-test

build:
	go build -o bin/pgb ./cmd/pgb
	go build -o bin/branchd ./cmd/branchd
	go build -o bin/pgbranch-github ./cmd/pgbranch-github

test:
	go test ./...

it:
	PGBRANCH_IT=1 go test ./... -count=1 -timeout 25m

k8s-it:
	PGBRANCH_K8S_IT=1 go test ./... -count=1 -timeout 30m

# Postgres version matrix: seed -> branch -> verify -> destroy per major
# (default "14 18"; override with PGBRANCH_MATRIX_VERSIONS="14 15 16 17 18").
# Pulls one postgres:<major> image per version.
matrix:
	PGBRANCH_MATRIX_IT=1 go test ./internal/engine/ -run Matrix -count=1 -v -timeout 25m

lint:
	go vet ./...

docker-build:
	docker build -t pgbranch/branchd:dev .

docker-build-ghook:
	docker build -f Dockerfile.ghook -t pgbranch/ghook:dev .

helm-test:
	hack/helm-test.sh
