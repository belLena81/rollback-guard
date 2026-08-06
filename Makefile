.PHONY: fmt-check test test-race vet lint contract-test phase3-integration phase4-integration phase5-integration

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

contract-test:
	go test ./test/contract/...

phase3-integration:
	go test -tags=integration ./test/integration/phase3/...

phase4-integration:
	go test -tags=integration -timeout=3m ./test/integration/phase4/...

phase5-integration:
	go test -tags=integration -timeout=3m ./test/integration/phase5/...
