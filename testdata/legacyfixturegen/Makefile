.PHONY: check

check:
	test -z "$$(gofmt -l .)"
	go mod tidy -diff
	go mod verify
	go test ./...
	go vet ./...
