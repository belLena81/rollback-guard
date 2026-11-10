# Rollback Guardian

Rollback Guardian is a Go-based agentic DevOps demo for one deliberately narrow workflow: detect an unhealthy Kubernetes rollout, investigate evidence, propose a rollback, require human approval, execute the rollback through scoped MCP tools, and verify recovery.

The project exists to demonstrate production-oriented Go architecture, Agent2Agent (A2A) delegation, Model Context Protocol (MCP) tool boundaries, Kubernetes automation, deterministic policy gates, and an inspectable audit timeline.

## Core Safety Rule

An agent recommendation never directly mutates infrastructure. Every rollback must pass through:

```text
A2A recommendation
deterministic validation
policy evaluation
human approval
MCP mutation
deterministic recovery verification
```

This rule is the main project invariant. The AI layer may diagnose and recommend; it may not approve, bypass policy, or directly call mutation tools.

## Services

- `cmd/orchestrator`: public REST API, incident lifecycle, approvals, audit timeline, and MCP client composition.
- `cmd/investigator`: A2A agent that gathers evidence and returns a structured diagnosis.
- `cmd/kubernetes-mcp`: separately deployed read-only and rollback MCP tool catalogs backed by `client-go`.
- `cmd/observability-mcp`: metrics and recovery-evaluation MCP tools.
- `cmd/demo-api`: intentionally breakable Kubernetes workload used for the local demo.

## Documentation Map

- [AGENTS.md](./AGENTS.md): shared agent instructions and restrictions.
- [SPEC.md](./SPEC.md): implementation contract and acceptance criteria.
- [DEVELOPMENT_PLAN.md](./DEVELOPMENT_PLAN.md): phase order and exit gates.
- [docs/adr.md](./docs/adr.md): architecture decisions.
- [docs/design.md](./docs/design.md): design notes and differentiators.
- [docs/feature-plan.md](./docs/feature-plan.md): feature backlog and product framing.
- [docs/local-demo-env.md](./docs/local-demo-env.md): local `kind` demo environment.
- [docs/tech-stack.md](./docs/tech-stack.md): selected libraries and alternatives.
- [.agents/00-INDEX.md](./.agents/00-INDEX.md): role briefs for agentic development.
- [docs/phase3-mcp.md](./docs/phase3-mcp.md), [docs/phase4-a2a.md](./docs/phase4-a2a.md), [docs/phase5-ai-diagnosis.md](./docs/phase5-ai-diagnosis.md), [docs/phase6-kind.md](./docs/phase6-kind.md), [docs/phase6-telemetry.md](./docs/phase6-telemetry.md): per-phase runtime configuration and verification.
- [CLAUDE.md](./CLAUDE.md): Claude Code guidance.
- [.devcontainer/README.md](./.devcontainer/README.md): containerised agent sandbox.

`AGENTS.md` and `.agents/` are local agent files and are not committed.

## Repository Shape

```text
cmd/
internal/
  domain/
  application/
  ports/
  adapters/
  platform/
  testkit/
api/
migrations/
deploy/
scripts/
test/
docs/
```

Phases 1 and 2 provide the domain lifecycle and durable REST/PostgreSQL surface. Phase 3 adds real typed MCP boundaries for Kubernetes and Prometheus. Phase 4 adds durable asynchronous A2A delegation to the standalone Investigator. Phase 5 adds provider-neutral LLM and hybrid diagnosis with deterministic rule fallback. Phase 6 is in progress: the `demo-api` workload, container images, kind manifests, the local demo flow, and telemetry (metrics, correlated logs, tracing) exist; retry/circuit-breaker decorators, the unattended end-to-end run, and CI do not yet. Phase 7 (authentication and users) has not started, so the API injects a fixed local actor and must stay on a trusted network.

## Canonical Choices

- Persistence: PostgreSQL with `pgx` and migration files.
- Architecture: ports and adapters with pure domain packages.
- Kubernetes access: `client-go` behind MCP servers, not generic shell access.
- A2A/MCP: official Go SDKs where practical.
- Diagnosis: rule-based by default; LLM and hybrid strategies behind the same port, always with rule fallback.
- Demo runtime: local `kind` cluster and purpose-built breakable `demo-api`.

## Development Commands

```bash
make test                # unit, contract, and in-process lifecycle tests
make test-race
make lint                # gofmt check, go vet, golangci-lint (also integration- and kind-tagged files)
make contract-test
make phase3-integration  # integration suites need Docker (testcontainers)
make phase4-integration
make phase5-integration
make postgres-integration  # Postgres adapter tests; a skipped test fails (needs ROLLBACK_GUARD_POSTGRES_TEST_DSN)
make integration         # every integration-tagged suite
make images              # service images and the four demo-api tags
make kind-create
make kind-load
make deploy
make demo-v1
make generate-load
make demo-v2-broken
make rbac-check
make kubeconfigs
make phase6-kind-check
make kind-delete
```

The kind targets are described in [docs/phase6-kind.md](./docs/phase6-kind.md). Still planned for Phase 6 and not implemented: `compose-up`, `compose-down`, `create-incident`, `approve-latest-action`, and `verify`.
