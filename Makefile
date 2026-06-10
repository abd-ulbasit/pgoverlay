.PHONY: build test it lint

build:
	go build -o bin/pgb ./cmd/pgb
	go build -o bin/branchd ./cmd/branchd

test:
	go test ./...

it:
	PGBRANCH_IT=1 go test ./... -count=1 -timeout 25m

lint:
	go vet ./...
