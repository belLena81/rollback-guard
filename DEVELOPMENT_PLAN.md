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

## Current Status

Phases 1-6 are complete. The Phase 6 exit gate is automated locally (`make e2e`, `make e2e-decline`) and in CI (the `e2e` job), and passed on a GitHub-hosted runner on 2026-11-10 (PR #1, commit `c0240b2`). Next: refactoring stages R4-R8 (`docs/refactoring-plan.md`), then Phase 7.

| Phase 6 task | State |
|---|---|
| `cmd/demo-api` and container images | done |
| kind manifests, RBAC ordering, local demo make targets (`docs/phase6-kind.md`) | done |
| Prometheus metrics, log correlation, OpenTelemetry tracing (`docs/phase6-telemetry.md`) | done |
| Retry/circuit-breaker decorators (`docs/phase6-resilience.md`) | done |
| Unattended E2E flow (`make e2e`, `e2e-decline`, `e2e-test`; `docs/local-demo-env.md`) | done |
| CI checks (`.github/workflows/ci.yml`, `docs/ci.md`) | done: lint, unit, contract, vuln, manifests, integration, images, and the kind e2e job (`make e2e`, `make e2e-decline`); written and validated locally, first hosted-runner run pending |

Code-quality refactoring is interleaved with the remaining tasks in the order given in `docs/refactoring-plan.md`.

The first real end-to-end run on kind found two defects that no earlier test could see, both fixed test-first with the E2E task:

- **Policy required an unhealthy rollout.** The rule diagnoser and the `current_rollout_unhealthy` policy rule only allowed a rollback while the rollout was incomplete. An error-storm build passes readiness, so its rollout completes and every such regression (the scenario in `SPEC.md`) was declined. Rollout health is no longer a rule. In its place, the new rule `rollout_recent` requires the rollout to lie within `RULE_CORRELATION_WINDOW` of the policy's own clock, so correlation no longer rests on the client-reported symptom start alone (an old healthy rollout cannot be made eligible by reporting a symptom start right after it). A stalled rollout without a breached metric is still declined (the decline scenario).
- **Recovery verification checked once, immediately.** Right after the rollout the metrics window still held the pre-rollback errors, so verification always failed. It now waits up to `RECOVERY_TIMEOUT` (default `METRICS_WINDOW` + 30s, capped at 7m, bounded by count and by the clock), checking every `RECOVERY_POLL_INTERVAL`, and treats a transient read failure during the wait as "not yet" (`docs/phase3-mcp.md`). The shared HTTP `WriteTimeout` rose from 10m to 15m so an approve request with the full wait still fits.
- **Recovery could pass without traffic.** The error-rate query returned `0` for a window with no requests. It now returns no sample (a failed read), so recovery cannot pass on a service that serves nothing (`docs/phase6-kind.md`). A healthy window without 5xx series still yields `0`.

Open findings from the E2E task:

- A rollout is dated by the current ReplicaSet's creation time. When a Deployment returns to a template it ran before, Kubernetes reuses the old ReplicaSet and its old timestamp, so a re-deployed bad template is declined as not recent (fail-safe). `make demo-reset` prunes old ReplicaSets so demo runs are repeatable; the product fix (a rollout start time that survives ReplicaSet reuse) is open.
- The threshold is still client-supplied: a very low reported threshold makes a small error rate a breach. A configured per-symptom minimum threshold, or a baseline comparison against the window before the rollout (`QueryRequest.Offset` exists), would anchor it server-side.
- Recovery accepts any non-zero traffic and the first passing window, which can still include samples from before the rollback. A minimum request rate, or a window that starts after the execution completed, would tighten it.
- `rollout_recent` is evaluated on the orchestrator's clock after the investigation, so a rollout near the window edge or a slow investigation is declined (fail-safe); rollout times up to `RolloutClockSkew` (30s) ahead are tolerated.
- Policy rule results are persisted (`policy_decisions.rules`) but not returned by the API, so a decline's reasons are visible only in the database.
- Two approve requests for one incident each run their own recovery wait; the version check lets only one commit (the other gets 409). A per-incident single-flight or lease would avoid the duplicate polling.
- The demo runs the investigator and, unless `ROLLBACK_MCP_DATABASE_URL` is set, the rollback MCP with the owner database role; `migrate` receives `DATABASE_URL` on its command line. Dedicated roles (`docs/phase3-mcp.md`) and passing the URL by environment are left to the deployment work.

Phase 7 has not started; its task breakdown and decisions are in `docs/phase7-auth.md`.

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

- `ports.Reasoner` and an LLM adapter behind the existing diagnoser boundary.
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
- `deploy/kubernetes` kustomize manifests and local `kind` setup. The four services run as local processes against the cluster (see `docs/phase6-kind.md`).
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
