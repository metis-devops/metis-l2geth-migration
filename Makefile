.PHONY: build test test-race vet lint fmt-check ci

build:
	go build -o bin/l2state ./cmd/l2state

lint:
	golangci-lint run 

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	test -z "$$(gofmt -l cmd internal)"
	go mod tidy -diff

ci: fmt-check vet lint test build
