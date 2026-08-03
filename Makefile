.PHONY: fmt-check test test-race vet lint

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go'))"

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

lint: fmt-check vet
	golangci-lint run ./...
