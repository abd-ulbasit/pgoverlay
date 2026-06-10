.PHONY: build test it lint

build:
	go build -o bin/pgb ./cmd/pgb

test:
	go test ./...

it:
	PGBRANCH_IT=1 go test ./... -count=1 -timeout 20m

lint:
	go vet ./...
