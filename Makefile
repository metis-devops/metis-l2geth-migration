.PHONY: build test test-race lint fmt-check fixture-check ci

build:
	go build -o bin/l2state ./cmd/l2state

lint:
	golangci-lint run 

test:
	go test ./...

test-race:
	go test -race ./...

fmt-check:
	test -z "$$(gofmt -l cmd internal)"
	go mod tidy -diff

fixture-check:
	cd testdata/legacyfixturegen && test -z "$$(gofmt -l .)"
	cd testdata/legacyfixturegen && go mod tidy -diff
	cd testdata/legacyfixturegen && go mod verify
	cd testdata/legacyfixturegen && go test ./...
	cd testdata/legacyfixturegen && go vet ./...

ci: fmt-check lint test fixture-check build
