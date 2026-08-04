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
- [.codex/README.md](./.codex/README.md): Codex-specific workflow and local skills.

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
demo/
test/
docs/
.agents/
```

Phases 1 and 2 provide the domain lifecycle and durable REST/PostgreSQL surface. Phase 3 adds real typed MCP boundaries for Kubernetes and Prometheus while preserving the same application ports. See [docs/phase3-mcp.md](./docs/phase3-mcp.md) for runtime configuration, RBAC, and verification.

## Canonical Choices

- Persistence: PostgreSQL with `pgx` and migration files.
- Architecture: ports and adapters with pure domain packages.
- Kubernetes access: `client-go` behind MCP servers, not generic shell access.
- A2A/MCP: official Go SDKs where practical.
- Diagnosis: rule-based first, LLM/hybrid later behind the same port.
- Demo runtime: local `kind` cluster and purpose-built breakable `demo-api`.

## Development Commands

The final project should provide:

```bash
make test
make test-race
make lint
make compose-up
make compose-down
make kind-create
make kind-delete
make images
make kind-load
make deploy
make demo-v1
make demo-v2-broken
make generate-load
make create-incident
make approve-latest-action
make verify
```

Until those targets exist, use the phase-specific tests described in `DEVELOPMENT_PLAN.md`.
