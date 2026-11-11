# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

Rollback Guardian is a Go demo of a safe agentic DevOps workflow: detect a bad Kubernetes rollout, investigate evidence, propose a rollback, require human approval bound to an exact proposal hash, execute through narrow MCP tools, and verify recovery. `AGENTS.md` (local, gitignored) holds the full agent rules. Read `SPEC.md`, `DEVELOPMENT_PLAN.md`, and the matching role brief in `.agents/` before working on a phase. Work proceeds strictly in phase order. Phases 1–7 are implemented (`DEVELOPMENT_PLAN.md`, Current Status; telemetry is described in `docs/phase6-telemetry.md`). Code-quality work follows `docs/refactoring-plan.md`. Phase 7 (GitHub OAuth, server-side sessions, roles `viewer` < `operator` < `administrator`) is described in `docs/phase7-auth.md`, including every auth env var (`AUTH_MODE=local|github`, `AUTH_GITHUB_*`, `AUTH_BOOTSTRAP_ADMINS`, `AUTH_STATE_SECRET`, `SESSION_TTL`, `SESSION_COOKIE_SECURE`) and its known gaps. `AUTH_MODE` defaults to `github` and the orchestrator refuses to start without the GitHub settings; dev/demo runs set `AUTH_MODE=local` explicitly (`scripts/demo/services.sh` does), which injects one fixed actor.

## Commands

```bash
make test              # go test ./...  (default unit gate)
make test-race
make lint              # gofmt check + go vet + golangci-lint, including integration- and kind-tagged files (config: .golangci.yml)
make contract-test     # test/contract: MCP tool catalogs, OpenAPI, RBAC manifest
make phase3-integration  # -tags=integration; needs Docker (testcontainers Postgres)
make phase4-integration
make phase5-integration
make postgres-integration  # Postgres adapter integration tests; fails on any skipped test (needs ROLLBACK_GUARD_POSTGRES_TEST_DSN)
make integration       # phase3-5 + postgres-integration (every integration-tagged suite)

# Single test
go test ./internal/domain -run TestName -v
go test -tags=integration ./test/integration/phase4/... -run TestName -v
```

Integration tests use the `//go:build integration` tag, so plain `go test ./...` skips them. The phase docs run every gate with `GOCACHE=/tmp/rollback-guard-go-cache` prefixed; use the same prefix if the default Go cache isn't writable. The kind demo targets (`kind-create`, `kind-load`, `deploy`, `demo-v1`, `demo-reset`, `demo-v2-broken`, `generate-load`, `rbac-check`, `kubeconfigs`, `phase6-kind-check`) are documented in `docs/phase6-kind.md`; the local stack and the end-to-end runs (`compose-up`, `services-up`/`services-down`/`services-status`, `create-incident`, `approve-latest-action`, `verify`, `e2e`, `e2e-decline`, `e2e-test` with build tag `e2e`) in `docs/local-demo-env.md`. The Makefile is the source of truth.

## Architecture

Ports-and-adapters with a pure domain:

- `internal/domain`: entities (`Incident`, `Evidence`, `InvestigationConclusion`, `RollbackProposal`, `Approval`, `Actor`, ...), the incident state machine (`incident.go`: transitions are explicit allow-lists; illegal ones return typed errors), eligibility policy, and `ApprovalAuthorizer`. Imports nothing from infrastructure: no adapters, SDKs, DB, HTTP, k8s, or LLM packages.
- `internal/ports/ports.go`: every interface the application depends on. Persistence goes through `UnitOfWork.WithinTransaction(ctx, func(Repositories) error)`, so a state change and its `OperationEvent` audit record commit atomically. Repositories use optimistic locking (`Update(entity, expectedVersion)`).
- `internal/application`: use-case services built by `NewServices` around one shared `workflow` (incidents, investigations, approvals, executions, verifications, queries). `InvestigationCoordinator` is a lease-based background worker. It claims durable `InvestigationDispatch` rows and submits and watches them through `ports.InvestigationGateway` (A2A). `ai_diagnosis.go` holds LLM/hybrid diagnosers that validate untrusted model output and fall back to the rule diagnoser.
- `internal/adapters/*`: `postgres` (pgx directly; no code generation), `memory` and `fake` (Phase 1 in-process fakes), `httpapi` (thin REST handlers, DTOs, JSON errors; contract in `api/openapi.yaml`), `mcp` (client adapters implementing the inspector, metrics, and executor ports, plus `kubernetesserver`/`observabilityserver` tool servers), `a2a` (gateway client, Investigator server, Postgres task store), `kubernetes` (client-go, deterministic revision hashing), `prometheus`, `diagnosis` (rule diagnoser), `llm` (OpenAI-compatible reasoner). SDK types (MCP, A2A, OpenAI, client-go) must not leave their adapter package.
- `internal/platform`: env config (`config.Load`; every default and bound is a named constant in the package (`defaults.go`, `resilience.go`)), `httpserver` (the one `Serve` with named timeouts, the one health handler, and the local auth middleware that injects a typed `Actor`), `bootstrap` (`NewPool`, `ConnectMCP`, `StartTelemetry` for the composition roots), `buildinfo` (the version string), clock/ID generator, `resilience` (retry and circuit-breaker decorators around external ports, applied only in `cmd/*`; `docs/phase6-resilience.md`), and `telemetry` (per-process Prometheus registry, OTel tracer provider, W3C propagation, correlated slog handler, metric decorators around ports). Application code sees telemetry only through `ports.Tracer` and `ports.Correlation`.
- `cmd/*/main.go`: composition roots only. Construct adapters, validate deps, wire services. Each `run()` is split into `buildMCP`, `buildServices`, and `buildHTTP`.

### Runtime topology

Four processes communicate over HTTP. Each binds `127.0.0.1` on the port below unless `HTTP_ADDR` is set (only the orchestrator authenticates, and only with `AUTH_MODE=github`; the investigator and MCP servers stay network-trusted; the service image sets `HTTP_ADDR=:8080`):

1. **orchestrator** (`:8080`): REST API plus Postgres. It connects to three MCP servers at startup and calls `RequireTools` to check each server's exact tool catalog, failing closed on any mismatch. For each incident it writes a dispatch record, returns `202`, and the coordinator delegates the investigation over A2A.
2. **investigator** (`:8094`): A2A agent (Agent Card at `/.well-known/agent-card.json`, REST at `/a2a`). It collects evidence through the read-only k8s MCP and the observability MCP, then diagnoses using `DIAGNOSIS_STRATEGY` = `rule_based` | `llm_based` | `hybrid`. It never approves or executes.
3. **kubernetes-mcp**: run twice with different service accounts. `KUBERNETES_MCP_MODE=readonly` (`:8091`) and `KUBERNETES_MCP_MODE=rollback` (`:8092`). In rollback mode it reads Postgres and re-checks the pending execution, proposal, policy decision, and approval before mutating. Its own `rollback_mutations` ledger enforces idempotency.
4. **observability-mcp** (`:8093`): Prometheus-backed error-rate, latency, and recovery tools.

Rollback path: diagnosis → deterministic validation → policy (`domain.EligibilityPolicy`) → human approval of the exact proposal hash → `execute_approved_rollback` MCP tool (takes only the proposal hash and idempotency key) → watch rollout → recovery evaluation → `resolved`. Revisions are SHA-256 digests of normalized Pod templates. The previous stable ReplicaSet is the one annotated `rollback-guardian.io/stable: "true"`.

Env vars, ports, and timeout constraints for each phase are documented in `docs/phase3-mcp.md`, `docs/phase4-a2a.md`, and `docs/phase5-ai-diagnosis.md`. Migrations are numbered pairs in `migrations/` and must be applied in order.

## Invariants (enforced by tests and review)

- No AI output may authorize or execute a Kubernetes mutation directly. LLM output is untrusted. It must pass domain construction, evidence-reference validation, and policy.
- MCP tools stay narrow and typed. Never add generic shell, generic `kubectl`, arbitrary patch/YAML/delete, pod exec, or secrets tools. `test/contract` pins the exact tool catalogs and RBAC (`deploy/rbac/mcp.yaml`).
- `Actor` is a typed domain value. Don't pass bare actor strings.
- Every external or blocking call takes `context.Context`. Constructors validate their dependencies. No global mutable state. No generic `utils`/`common` packages. Interfaces are small and consumer-owned.
- Never log or persist secrets, DB URLs, auth headers, raw LLM prompts, or provider bodies/errors.
- Use TDD with table-driven tests for domain and state-machine rules. If a port turns out to be insufficient, fix the port and the earlier tests rather than adding a parallel workaround.
- Don't modify `.idea/` or generated files.

## Agent sandbox

Integration work (PostgreSQL, testcontainers, kind, Prometheus, optional Ollama) runs in the containerised sandbox defined in `.devcontainer/` (guide: `.devcontainer/README.md`). It operates on a copy of the repo and its own Docker daemon. Start it with `./.devcontainer/sandbox.sh up`. `IS_SANDBOX=1` means you are already inside. Project skills live in `.claude/skills/` (`start-phase`, `verify-gates`, `add-port-adapter`, `mcp-tool-change`, `postgres-migration`, `invariant-review`, `sandbox`) and reviewer subagents in `.claude/agents/`. Delegated sandbox tasks and subagents use model tiers (heavy/standard/light) defined in `.devcontainer/README.md`, "Model routing".
