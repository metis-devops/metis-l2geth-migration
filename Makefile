.PHONY: build test test-race test-race-root lint fmt-check fixture-check legacy-prune-check geth-compat geth-compat-candidate ci ci-root

build:
	go build -o bin/l2state ./cmd/l2state

lint:
	golangci-lint run 

test:
	go test ./...

test-race: test-race-root
	$(MAKE) -C testdata/legacyprune test-race

test-race-root:
	go test -race ./...

fmt-check:
	test -z "$$(gofmt -l cmd internal)"
	go mod tidy -diff

fixture-check:
	$(MAKE) -C testdata/legacyfixturegen check

legacy-prune-check:
	$(MAKE) -C testdata/legacyprune check

geth-compat:
	go test -count=1 ./internal/migration -run '^TestGethCompatibility'

# OUT must name a new directory outside the committed baseline corpus.
# It is passed through the environment so shell metacharacters are not evaluated.
export L2STATE_GETH_COMPAT_OUT = $(OUT)
geth-compat-candidate:
	@test -n "$$L2STATE_GETH_COMPAT_OUT" || (echo 'OUT must name a new candidate directory' >&2; exit 1)
	go test -count=1 -v ./internal/migration -run '^TestWriteGethCompatibilityCandidate$$'

ci-root: fmt-check lint test build

ci: ci-root fixture-check legacy-prune-check
