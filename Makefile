.PHONY: build test test-race lint fmt-check fixture-check geth-compat geth-compat-candidate ci

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

geth-compat:
	go test -count=1 ./internal/migration -run '^TestGethCompatibility'

# OUT must name a new directory outside the committed baseline corpus.
# It is passed through the environment so shell metacharacters are not evaluated.
export L2STATE_GETH_COMPAT_OUT = $(OUT)
geth-compat-candidate:
	@test -n "$$L2STATE_GETH_COMPAT_OUT" || (echo 'OUT must name a new candidate directory' >&2; exit 1)
	go test -count=1 -v ./internal/migration -run '^TestWriteGethCompatibilityCandidate$$'

ci: fmt-check lint test fixture-check build
