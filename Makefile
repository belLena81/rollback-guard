.PHONY: fmt-check test test-race vet lint contract-test phase3-integration phase4-integration phase5-integration images images-services images-demo-api

DOCKER ?= docker
IMAGE_PREFIX ?= rollback-guardian
IMAGE_TAG ?= dev
SERVICES := orchestrator investigator kubernetes-mcp observability-mcp
# <image tag>:<DEMO_MODE>:<DEMO_VERSION>, tags from docs/local-demo-env.md.
DEMO_VARIANTS := v1-healthy:healthy:v1 v2-bad-crashloop:crashloop:v2 v2-bad-error-storm:error-storm:v2 v2-bad-high-latency:high-latency:v2

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

images: images-services images-demo-api

images-services:
	@set -e; for service in $(SERVICES); do \
		echo "==> $(IMAGE_PREFIX)/$$service:$(IMAGE_TAG)"; \
		$(DOCKER) build -f deploy/docker/service.Dockerfile --build-arg SERVICE=$$service -t $(IMAGE_PREFIX)/$$service:$(IMAGE_TAG) .; \
	done

images-demo-api:
	@set -e; for variant in $(DEMO_VARIANTS); do \
		tag=$${variant%%:*}; rest=$${variant#*:}; mode=$${rest%%:*}; version=$${rest#*:}; \
		echo "==> demo-api:$$tag (DEMO_MODE=$$mode DEMO_VERSION=$$version)"; \
		$(DOCKER) build -f deploy/docker/demo-api.Dockerfile --build-arg DEMO_MODE=$$mode --build-arg DEMO_VERSION=$$version -t demo-api:$$tag .; \
	done
