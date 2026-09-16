.PHONY: check test-race

check:
	test -z "$$(gofmt -l .)"
	go mod tidy -diff
	go mod verify
	go test -count=1 ./...
	go vet ./...

test-race:
	L2STATE_TEST_RACE=1 go test -race -count=1 ./...
