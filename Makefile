.PHONY: fmt-check test test-race vet lint contract-test phase3-integration phase4-integration phase5-integration postgres-integration integration phase6-telemetry images images-services images-demo-api \
	kind-create kind-delete kind-load deploy demo-v1 demo-v2-broken generate-load rbac-check kubeconfigs phase6-kind-check

# Build tags of every tagged test file, so vet and lint also cover the
# integration and kind suites that plain `go test ./...` skips.
LINT_TAGS ?= integration,kind

DOCKER ?= docker
IMAGE_PREFIX ?= rollback-guardian
IMAGE_TAG ?= dev
SERVICES := orchestrator investigator kubernetes-mcp observability-mcp
# <image tag>:<DEMO_MODE>:<DEMO_VERSION>, tags from docs/local-demo-env.md.
DEMO_VARIANTS := v1-healthy:healthy:v1 v2-bad-crashloop:crashloop:v2 v2-bad-error-storm:error-storm:v2 v2-bad-high-latency:high-latency:v2
DEMO_IMAGES := $(foreach variant,$(DEMO_VARIANTS),demo-api:$(firstword $(subst :, ,$(variant))))

# Local kind demo (docs/phase6-kind.md). Every value is overridable.
KIND ?= kind
KUBECTL ?= kubectl
KIND_CLUSTER ?= rollback-guard
# Every kubectl call is pinned to this context so the demo targets can never
# act on whatever cluster happens to be current.
KUBE_CONTEXT ?= kind-$(KIND_CLUSTER)
K8S = $(KUBECTL) --context $(KUBE_CONTEXT)
KIND_CONFIG ?= deploy/kind/cluster.yaml
DEMO_NAMESPACE ?= rollback-demo
DEMO_DEPLOYMENT ?= demo-api
# Broken variant for demo-v2-broken: error-storm | high-latency | crashloop.
DEMO_BROKEN ?= error-storm
DEMO_ROLLOUT_TIMEOUT ?= 120s
# NodePorts are published on the Docker daemon's host: localhost for a local
# daemon, <host> for DOCKER_HOST=tcp://<host>:<port>.
DOCKER_DAEMON_HOST := $(firstword $(subst :, ,$(patsubst tcp://%,%,$(filter tcp://%,$(DOCKER_HOST)))))
DEMO_HOST ?= $(or $(DOCKER_DAEMON_HOST),localhost)
DEMO_URL ?= http://$(DEMO_HOST):30080
LOAD_DURATION ?= 60
LOAD_CONCURRENCY ?= 4
# Service-account kubeconfigs for the host-run Kubernetes MCP processes.
DEMO_STATE_DIR ?= .demo
SA_TOKEN_DURATION ?= 8h

fmt-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go'))"

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet -tags=$(LINT_TAGS) ./...

lint: fmt-check vet
	golangci-lint run --build-tags=$(LINT_TAGS) ./...

contract-test:
	go test ./test/contract/...

phase3-integration:
	go test -tags=integration ./test/integration/phase3/...

phase4-integration:
	go test -tags=integration -timeout=3m ./test/integration/phase4/...

phase5-integration:
	go test -tags=integration -timeout=3m ./test/integration/phase5/...

# PostgreSQL adapter integration tests (repositories, rollback mutation guard).
# They need ROLLBACK_GUARD_POSTGRES_TEST_DSN, which the sandbox sets, and skip
# without it. A skipped test proves nothing, so any skip fails this target.
postgres-integration:
	bash scripts/ci/test-no-skips.sh -tags=integration ./internal/adapters/postgres/...

# Every integration-tagged suite. The kind-tagged suite needs a cluster and
# runs separately as phase6-kind-check.
integration: phase3-integration phase4-integration phase5-integration postgres-integration

# In-process telemetry exit check (docs/phase6-telemetry.md); also part of `make test`.
phase6-telemetry:
	go test -race ./test/phase6/... ./internal/platform/telemetry/...

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

kind-create:
	@if $(KIND) get clusters | grep -qx '$(KIND_CLUSTER)'; then \
		echo "kind cluster $(KIND_CLUSTER) already exists"; \
	else \
		$(KIND) create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG) --wait 120s; \
	fi

kind-delete:
	$(KIND) delete cluster --name $(KIND_CLUSTER)

kind-load:
	$(KIND) load docker-image --name $(KIND_CLUSTER) $(DEMO_IMAGES)

deploy:
	$(K8S) apply -k deploy/kubernetes
	$(K8S) -n $(DEMO_NAMESPACE) rollout status deployment/$(DEMO_DEPLOYMENT) --timeout=$(DEMO_ROLLOUT_TIMEOUT)

demo-v1:
	$(K8S) -n $(DEMO_NAMESPACE) set image deployment/$(DEMO_DEPLOYMENT) demo-api=demo-api:v1-healthy
	$(K8S) -n $(DEMO_NAMESPACE) rollout status deployment/$(DEMO_DEPLOYMENT) --timeout=$(DEMO_ROLLOUT_TIMEOUT)
	KUBECTL=$(KUBECTL) KUBE_CONTEXT=$(KUBE_CONTEXT) bash scripts/demo/mark-stable.sh $(DEMO_NAMESPACE) $(DEMO_DEPLOYMENT) demo-api:v1-healthy

demo-v2-broken:
	$(K8S) -n $(DEMO_NAMESPACE) set image deployment/$(DEMO_DEPLOYMENT) demo-api=demo-api:v2-bad-$(DEMO_BROKEN)
	@if [ "$(DEMO_BROKEN)" = crashloop ]; then \
		echo "crashloop never becomes ready; the rollout is left stalled on purpose"; \
	else \
		$(K8S) -n $(DEMO_NAMESPACE) rollout status deployment/$(DEMO_DEPLOYMENT) --timeout=$(DEMO_ROLLOUT_TIMEOUT); \
	fi

generate-load:
	bash scripts/demo/generate-load.sh $(DEMO_URL) $(LOAD_DURATION) $(LOAD_CONCURRENCY)

phase6-kind-check: kubeconfigs
	KIND_READONLY_KUBECONFIG=$(abspath $(DEMO_STATE_DIR))/kubeconfig-readonly \
	KIND_ROLLBACK_KUBECONFIG=$(abspath $(DEMO_STATE_DIR))/kubeconfig-rollback \
	go test -tags=kind -count=1 ./test/integration/phase6/...

rbac-check:
	KUBECTL=$(KUBECTL) KUBE_CONTEXT=$(KUBE_CONTEXT) bash scripts/demo/rbac-check.sh $(DEMO_NAMESPACE)

kubeconfigs:
	KUBECTL=$(KUBECTL) KUBE_CONTEXT=$(KUBE_CONTEXT) bash scripts/demo/sa-kubeconfig.sh $(DEMO_NAMESPACE) rollback-guardian-kubernetes-readonly $(SA_TOKEN_DURATION) $(DEMO_STATE_DIR)/kubeconfig-readonly
	KUBECTL=$(KUBECTL) KUBE_CONTEXT=$(KUBE_CONTEXT) bash scripts/demo/sa-kubeconfig.sh $(DEMO_NAMESPACE) rollback-guardian-kubernetes-rollback $(SA_TOKEN_DURATION) $(DEMO_STATE_DIR)/kubeconfig-rollback
