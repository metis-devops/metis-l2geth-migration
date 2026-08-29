.PHONY: build test test-race lint fmt-check ci

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

ci: fmt-check lint test build
