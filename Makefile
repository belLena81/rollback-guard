.PHONY: fmt-check test test-race vet lint contract-test phase3-integration phase4-integration phase5-integration postgres-integration integration phase6-telemetry images images-services images-demo-api \
	kind-create kind-delete kind-load deploy demo-v1 demo-v2-broken generate-load rbac-check kubeconfigs phase6-kind-check \
	demo-reset services-build migrate-up services-up services-down services-status compose-up compose-down \
	create-incident approve-latest-action verify e2e e2e-decline e2e-test auth-login

# Build tags of every tagged test file, so vet and lint also cover the
# integration and kind suites that plain `go test ./...` skips.
LINT_TAGS ?= integration,kind,e2e

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

# Local service processes (make services-up, docs/local-demo-env.md).
SERVICES_BIN_DIR ?= bin/demo
MIGRATE ?= migrate
# The demo uses a short metrics window so detection and recovery are visible
# within a minute of a rollout instead of five (domain minimum: 30s).
METRICS_WINDOW ?= 30s
# local (default): services.sh sets AUTH_MODE=local explicitly (the orchestrator
# itself defaults to github): the fixed local actor, no login. github: the orchestrator runs
# AUTH_MODE=github against a fake GitHub and the demo scripts use real sessions
# (docs/phase7-auth.md, "End-to-end with a real session"). The e2e targets force github.
DEMO_AUTH ?= local
SERVICES_ENV = DEMO_AUTH=$(DEMO_AUTH) DEMO_STATE_DIR=$(DEMO_STATE_DIR) SERVICES_BIN_DIR=$(SERVICES_BIN_DIR) METRICS_WINDOW=$(METRICS_WINDOW)
COMPOSE ?= docker compose
COMPOSE_FILE ?= deploy/compose/compose.yaml
# Set to `--profile tracing` to also start Jaeger (OTLP on 4318, UI/API on 16686).
COMPOSE_PROFILES ?=

# REST demo steps (create-incident, approve-latest-action, verify, e2e).
ORCHESTRATOR_URL ?= http://127.0.0.1:8080
INCIDENT_THRESHOLD ?= 0.1
# verify: `resolved` (default) or `declined`.
VERIFY_EXPECT ?= resolved
STABLE_IMAGE ?= demo-api:v1-healthy
DEMO_API_ENV = DEMO_AUTH=$(DEMO_AUTH) DEMO_STATE_DIR=$(DEMO_STATE_DIR) ORCHESTRATOR_URL=$(ORCHESTRATOR_URL) \
	KUBECTL=$(KUBECTL) KUBE_CONTEXT=$(KUBE_CONTEXT) DEMO_NAMESPACE=$(DEMO_NAMESPACE) DEMO_DEPLOYMENT=$(DEMO_DEPLOYMENT) \
	DEMO_URL=$(DEMO_URL) METRICS_WINDOW=$(METRICS_WINDOW) INCIDENT_THRESHOLD=$(INCIDENT_THRESHOLD) STABLE_IMAGE=$(STABLE_IMAGE)

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
# The package also has untagged tests, so one test from each tagged file is
# required by name: a dropped tag or file cannot pass on the untagged ones.
postgres-integration:
	REQUIRED_TESTS="TestEventAppendReportsSequenceConflictToSecondWriter TestRollbackMutationGuardBlocksReplayUntilCompletion TestApproveAndExecuteConcurrentSameApprovalOnPostgres TestAuthResolveActiveSession TestAuthMigrationDown TestSessionAuthMiddlewareOnPostgres TestGitHubLoginOnPostgres TestAuthAuditAppendListAndShape TestAuthAuditMigrationDown TestConcurrentAdminDemotionsKeepOneAdministrator TestAdminDisableEnableUserOnPostgres TestAuthAuditUserStatusMigrationDown TestDisableAuditFailureRollsBackOnPostgres TestGitHubLoginRoleEnforcementAndAuditOnPostgres TestRollbackMutationGuardRechecksGitHubApproverAtExecution TestRollbackMutationGuardLocalModeAcceptsOnlyTheLocalActor TestApprovalRecordedAcrossADemotionCannotExecute TestAuthReadTransactionReadsButCannotWrite TestAuthAuditListPageWalksTiedTimestamps" \
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

# Return the demo to a clean v1 state: roll to v1-healthy, mark it stable, and
# delete the scaled-down ReplicaSets of earlier broken rollouts so the next
# demo-v2-broken creates a fresh ReplicaSet (docs/local-demo-env.md, "Reset").
demo-reset: demo-v1
	KUBECTL=$(KUBECTL) KUBE_CONTEXT=$(KUBE_CONTEXT) bash scripts/demo/prune-replicasets.sh $(DEMO_NAMESPACE) $(DEMO_DEPLOYMENT)

# The five Rollback Guardian processes as local binaries against the kind
# cluster (scripts/demo/services.sh). DATABASE_URL and PROMETHEUS_URL come
# from the environment (the sandbox sets both; `make compose-up` provides
# them elsewhere, see docs/local-demo-env.md).
services-build:
	@mkdir -p $(SERVICES_BIN_DIR)
	@set -e; for service in $(SERVICES); do \
		echo "==> go build $(SERVICES_BIN_DIR)/$$service"; \
		go build -o $(SERVICES_BIN_DIR)/$$service ./cmd/$$service; \
	done
	@echo "==> go build $(SERVICES_BIN_DIR)/fake-github (test tool, never in an image)"
	@go build -o $(SERVICES_BIN_DIR)/fake-github ./test/tools/fake-github

# The command line names $$DATABASE_URL, never its value.
migrate-up:
	@test -n "$$DATABASE_URL" || { echo "DATABASE_URL is required" >&2; exit 1; }
	@$(MIGRATE) -path migrations -database "$$DATABASE_URL" up

services-up: services-build migrate-up kubeconfigs
	$(SERVICES_ENV) bash scripts/demo/services.sh up

services-down:
	$(SERVICES_ENV) bash scripts/demo/services.sh down

services-status:
	$(SERVICES_ENV) bash scripts/demo/services.sh status

# PostgreSQL, Prometheus (scraping demo-api on the kind node), and an optional
# Jaeger for developers outside the agent sandbox. Prometheus joins the
# `kind` Docker network, so run `make kind-create` first.
compose-up:
	$(COMPOSE) -f $(COMPOSE_FILE) $(COMPOSE_PROFILES) up -d --wait

compose-down:
	$(COMPOSE) -f $(COMPOSE_FILE) --profile tracing down

# Scripted REST steps of the demo (scripts/demo/{create-incident,approve-latest-action,verify}.sh).
create-incident:
	$(DEMO_API_ENV) bash scripts/demo/create-incident.sh

approve-latest-action:
	$(DEMO_API_ENV) bash scripts/demo/approve-latest-action.sh

verify:
	$(DEMO_API_ENV) bash scripts/demo/verify.sh $(VERIFY_EXPECT)

# Unattended Phase 6/7 exit gate, driven by real sessions against a fake GitHub:
# healthy -> load -> broken -> threshold in
# Prometheus -> incident -> awaiting_approval -> approve -> verify.
e2e e2e-decline e2e-test: DEMO_AUTH := github
e2e: services-up
	$(DEMO_API_ENV) MAKE="$(MAKE)" bash scripts/demo/e2e.sh rollback

# Policy-decline scenario: a crashlooping rollout that never serves errors is
# declined without any mutation (docs/local-demo-env.md, "Decline scenario").
e2e-decline: services-up
	$(DEMO_API_ENV) MAKE="$(MAKE)" bash scripts/demo/e2e.sh decline

# The same flow as an automated Go test against a running stack (make
# services-up first). The e2e build tag keeps it out of `go test ./...`.
# The test output is captured and scanned for every session token: the ones
# in the auth-tokens file and the ones the test itself minted.
E2E_TEST_LOG = $(DEMO_STATE_DIR)/e2e-test.log
# Absolute: go test runs the package with its working directory at test/e2e.
E2E_MINTED_TOKENS = $(abspath $(DEMO_STATE_DIR)/e2e-minted-tokens)
e2e-test:
	@mkdir -p $(DEMO_STATE_DIR) && rm -f $(E2E_MINTED_TOKENS)
	@# The scans run even when the tests fail (a leak matters most then); the
	@# test's exit status is kept and the minted-token file is always removed.
	@rc=0; \
	$(DEMO_API_ENV) MAKE="$(MAKE)" E2E_MINTED_TOKENS_FILE=$(E2E_MINTED_TOKENS) bash -o pipefail -c \
		'go test -tags=e2e -count=1 -timeout=40m -v ./test/e2e/... 2>&1 | tee $(E2E_TEST_LOG)' || rc=$$?; \
	$(DEMO_API_ENV) bash scripts/demo/check-no-tokens.sh $(E2E_TEST_LOG) || rc=1; \
	if [ -s $(E2E_MINTED_TOKENS) ]; then \
		$(DEMO_API_ENV) AUTH_TOKENS_FILE=$(E2E_MINTED_TOKENS) bash scripts/demo/check-no-tokens.sh $(E2E_TEST_LOG) || rc=1; \
	elif [ $$rc -eq 0 ]; then \
		echo "e2e-test: no minted tokens were recorded" >&2; rc=1; \
	fi; \
	rm -f $(E2E_MINTED_TOKENS); exit $$rc

# Signs the fake GitHub users in again (fresh session tokens in .demo/auth-tokens)
# on a stack started with DEMO_AUTH=github.
auth-login:
	$(DEMO_API_ENV) bash scripts/demo/auth-login.sh all
