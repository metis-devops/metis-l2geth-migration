.PHONY: build test test-race lint fmt-check fixture-check legacy-compat-check ci

build:
	go build -o bin/l2state ./cmd/l2state

lint:
	golangci-lint run 

test:
	go test ./...

test-race:
	go test -race ./...
	cd testdata/legacycompat && go test -race -count=1 ./...

fmt-check:
	test -z "$$(gofmt -l cmd internal)"
	go mod tidy -diff

fixture-check:
	cd testdata/legacyfixturegen && test -z "$$(gofmt -l .)"
	cd testdata/legacyfixturegen && go mod tidy -diff
	cd testdata/legacyfixturegen && go mod verify
	cd testdata/legacyfixturegen && go test ./...
	cd testdata/legacyfixturegen && go vet ./...

legacy-compat-check:
	cd testdata/legacycompat && test -z "$$(gofmt -l .)"
	cd testdata/legacycompat && go mod tidy -diff
	cd testdata/legacycompat && go mod verify
	cd testdata/legacycompat && go test -count=1 ./...
	cd testdata/legacycompat && go vet ./...

ci: fmt-check lint test fixture-check legacy-compat-check build
