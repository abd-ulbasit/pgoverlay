.PHONY: build test it k8s-it lint docker-build docker-build-ghook helm-test

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

lint:
	go vet ./...

docker-build:
	docker build -t pgbranch/branchd:dev .

docker-build-ghook:
	docker build -f Dockerfile.ghook -t pgbranch/ghook:dev .

helm-test:
	hack/helm-test.sh
