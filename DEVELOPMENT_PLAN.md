# Rollback Guardian Development Plan

## Development Philosophy

Build the system in phases so every phase ends runnable, testable, and demoable. Keep domain logic and ports stable early, then add infrastructure adapters without rewrites. Identity is modeled from Phase 1 as a typed `Actor`, but real authentication is deliberately last.

## Phase Overview

| Phase | Output | New Dependency |
|---|---|---|
| 1. Domain Core | State machine, ports, fakes, rule diagnosis | none |
| 2. REST API & Persistence | HTTP API, PostgreSQL, config, health | PostgreSQL |
| 3. MCP Integration | Kubernetes and observability MCP servers | MCP SDK, `client-go` |
| 4. A2A Integration | Investigator as standalone A2A service | A2A SDK |
| 5. AI Diagnosis | LLM and hybrid diagnosers | LLM provider |
| 6. Real Kubernetes & Production Qualities | `kind`, demo workload, telemetry, CI | kind, Helm/Compose, OTel |
| 7. Auth & Users | GitHub OAuth, sessions, role policy | OAuth |

## Phase 1: Domain Core

Goal: prove the full lifecycle in process, against fakes.

Build:

- `internal/domain`: incident, symptom, evidence, conclusion, proposal, approval, actor, state machine, typed errors.
- `internal/ports`: repository, evidence, deployment inspection, metrics, diagnoser, rollback executor, approval/policy interfaces.
- In-memory repositories and fake infrastructure ports.
- Rule-based diagnoser and rollback eligibility rules.
- Optimistic locking semantics even for in-memory storage.

Exit gate:

- An in-process test creates an incident, investigates fake evidence, generates a rollback proposal, approves it, executes fake rollback, verifies fake recovery, reaches `resolved`, and records an ordered audit trail.

Do not add HTTP, database, MCP, A2A, LLM, or real auth.

## Phase 2: REST API & Persistence

Goal: make Phase 1 durable and reachable over HTTP without changing domain behavior.

Build:

- `cmd/orchestrator` with REST endpoints from `SPEC.md`.
- PostgreSQL migrations and `pgx` repository adapters.
- Transactional state mutation plus audit-event writes.
- No-op auth middleware that injects `Actor{ID: "local-admin", Kind: ActorHuman}`.
- Config, health, ready, and placeholder metrics endpoints.

Exit gate:

- The Phase 1 workflow is drivable through HTTP, survives restart, and `GET /v1/incidents/{id}/events` returns the ordered audit trail.

Do not add real MCP, A2A, LLM, or users.

## Phase 3: MCP Integration

Goal: replace fake infrastructure reads/mutations with real MCP tools behind the same ports.

Build:

- `cmd/kubernetes-mcp` with only the approved Kubernetes tools.
- `cmd/observability-mcp` with only typed metrics/recovery tools.
- MCP client adapters implementing Phase 1 ports.
- Namespace-scoped RBAC manifests.
- Tool schema and error-mapping contract tests.

Exit gate:

- The HTTP workflow produces evidence and executes approved rollback through MCP against a test cluster while callers still use the same ports.

Do not introduce generic shell, arbitrary `kubectl`, arbitrary patch, arbitrary YAML, or delete tools.

## Phase 4: A2A Integration

Goal: extract investigation into an independent A2A service without changing diagnosis behavior.

Build:

- `cmd/investigator` with Agent Card and A2A endpoint.
- Orchestrator A2A client adapter.
- Task correlation, idempotency, progress states, timeout, and cancellation handling.
- Contract tests for Agent Card, task, progress, and result shape.

Exit gate:

- Orchestrator, Investigator, and both MCP servers cooperate over real A2A and MCP and reach the same resolved outcome as Phase 3.

## Phase 5: AI Diagnosis

Goal: add LLM and hybrid strategies without making correctness depend on a model.

Build:

- `ReasonerPort`/LLM adapter behind the existing diagnoser boundary.
- Rule-based, LLM-based, and hybrid strategies selected by config.
- Structured output validation outside the LLM adapter.
- Deterministic fallback when the LLM is unavailable or invalid.
- Prompt/model metadata capture.

Exit gate:

- Switching diagnosis strategy changes only the diagnosis path; proposal, approval, execution, and verification behavior remain unchanged.

## Phase 6: Real Kubernetes & Production Qualities

Goal: make the local demo and operational baseline complete.

Build:

- `cmd/demo-api` with healthy and broken image tags.
- `deploy/kubernetes`, `deploy/helm` or equivalent manifests, and local `kind` setup.
- Prometheus metrics, structured logging, OpenTelemetry tracing.
- Retry/circuit-breaker decorators around transient external ports.
- Make targets and CI checks.
- Full unattended E2E flow.

Exit gate:

- A fresh `kind` environment can run the full demo: deploy healthy, generate load, deploy broken, detect/investigate/propose/approve/rollback/verify, and prove audit/tracing evidence.

## Phase 7: Auth & Users

Goal: replace hardcoded actor injection with real identity without changing domain model.

Build:

- GitHub OAuth login.
- Server-side sessions and revocation.
- Users table, roles, organization allow-list.
- Real auth middleware implementation.
- Role-based policy checks for approval.

Exit gate:

- A real authorized user can create an incident, a sufficiently privileged user can approve production rollback, audit events use real actors, and session revocation takes effect immediately.

## Cross-Phase Invariants

1. `internal/domain` never imports adapters.
2. Protocol SDK types never escape adapter packages.
3. `Actor` is always a typed struct, never a bare string.
4. State mutation and audit event persist atomically once a real database exists.
5. No MCP tool broader than `SPEC.md` permits exists, even for debugging.
6. No agent recommendation directly authorizes Kubernetes mutation.

## Agentic Execution Rules

- Complete one phase exit gate before starting the next phase.
- Re-read the corresponding `SPEC.md` sections before coding each phase.
- If a port is insufficient, fix the port and earlier tests instead of creating a parallel workaround.
- Run the full accumulated test suite at the end of every phase.
