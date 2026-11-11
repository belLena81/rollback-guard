# Rollback Guardian Architecture Decisions

Status: accepted for v1 unless marked superseded.

## ADR-01: Use Official A2A Go SDK

Use the official `github.com/a2aproject/a2a-go/v2` SDK for inter-agent communication.

Amended: A2A carries only the investigation task. Human approval is not modeled as A2A task state; it is an Orchestrator REST command bound to the proposal hash, because the Investigator must never take part in approval or execution.

Enforced: the Investigator emits only `submitted`, `working`, `completed`, `failed`, and `canceled`. If an investigation task enters `input-required` or `auth-required`, the Orchestrator treats it as a protocol violation (`ports.ErrInvestigationProtocolViolation`): it stops watching, cancels the remote task, and fails the investigation as `invalid_investigation_result` without retrying. The Investigator's composition root and configuration have no rollback, approval, or auth surface. Pinned by `TestTaskStatesAreClassified`, `TestGatewayWatchFailsFastWhenInvestigatorAsksForInput`, `TestInvestigatorEmitsOnlyLifecycleStates` (`internal/adapters/a2a`), `TestCoordinatorFailsInvestigationOnProtocolViolationWithoutRetry` (`internal/application`), and `TestInvestigatorHasNoPathToRollbackOrApproval` / `TestInvestigatorConfigHasNoRollbackOrApprovalSettings` (`test/contract`).

## ADR-02: Use MCP as a Capability Boundary

Expose infrastructure access through narrow MCP servers:

- Kubernetes read-only MCP (`cmd/kubernetes-mcp`, `KUBERNETES_MCP_MODE=readonly`): read deployment, ReplicaSets, pods, events, rollout state.
- Observability MCP (`cmd/observability-mcp`): read typed service metrics and recovery signals.
- Kubernetes rollback MCP (`cmd/kubernetes-mcp`, `KUBERNETES_MCP_MODE=rollback`): prepare and execute only approved rollback operations.

Do not expose generic shell, `kubectl`, arbitrary patch, arbitrary YAML, or delete tools.

## ADR-03: Separate Agent Responsibilities

Use single-responsibility services: Orchestrator, Investigator, Kubernetes MCP, Observability MCP, and Demo API. The Orchestrator owns lifecycle and approval; the Investigator owns evidence-linked diagnosis; MCP servers own scoped tools.

## ADR-04: Mandatory Governance Gate

Rollback never executes without deterministic validation, policy evaluation, and explicit human approval. High model confidence may justify requesting approval; it never replaces approval.

## ADR-05: Provider-Neutral Reasoning

LLM reasoning lives behind a small port. The system starts with deterministic rules, then adds LLM and hybrid strategies. Invalid or unavailable LLM output falls back to rules.

## ADR-06: Ports and Adapters

Domain packages are pure. Application services depend on ports. Protocol, Kubernetes, Prometheus, PostgreSQL, and LLM SDK types stay inside adapter packages. Use manual constructor injection.

## ADR-07: Events and Audit

Emit immutable, past-tense domain events and persist every state-changing step to the audit timeline. The audit timeline is both operational evidence and the source for the transparency view. Today it is exposed as `GET /v1/incidents/{incidentId}/events`; no UI exists.

## ADR-08: PostgreSQL Persistence

Use PostgreSQL with `pgx` and migrations as the v1 persistence mechanism. Earlier source notes proposed pure-Go SQLite for zero-friction local runs, but the technical specification and development plan require PostgreSQL for relational integrity, transaction boundaries, and realistic integration tests.

SQLite may be reconsidered only as an explicitly separate local-only profile after the PostgreSQL path is complete.

## ADR-09: Replayable Timeline

Build an inspectable evidence-to-decision timeline: evidence gathered, hypothesis and confidence, policy decision, approval outcome, execution, and verification. This is a primary differentiator, not optional polish.

## ADR-10: Purpose-Built Demo Workload

Use a tiny Go HTTP service with healthy and broken image tags. Rollback should use real Kubernetes rollout semantics, not a hidden runtime flag reset.

## ADR-11: Kubernetes Access Through `client-go`

MCP servers use `client-go` directly and run with namespace-scoped RBAC. The read-only service account must not be able to patch deployments; the rollback account must not have cluster-wide or secret access.

## ADR-12: Layered Tests

Use fast unit tests by default, then contract tests for protocol shape, integration tests for PostgreSQL/MCP/A2A behavior, and gated `kind` E2E tests for the full workflow.

## ADR-13: Explicit Non-Goals

Do not build multi-tenant production incident management, autonomous rollback without approval, generic notification integrations, high availability, or production secrets management in v1.

## ADR-14: Demo Services Run Beside the Cluster

For the local demo only `demo-api` runs inside `kind`. The Orchestrator, Investigator, and MCP servers run as local processes next to PostgreSQL and Prometheus. Each Kubernetes MCP process authenticates with a short-lived token for its own ServiceAccount, so the API server still enforces the least-privilege RBAC. In-cluster deployment of the services is not required for v1.

## ADR-15: Stable Revision Is an Operator Mark on the ReplicaSet

The rollback target is the single ReplicaSet annotated `rollback-guardian.io/stable: "true"`. The mark is an explicit operator action after a release is verified good. It is never placed on the Deployment or its Pod template, because Deployment annotations are copied to every new ReplicaSet and template annotations change the revision digest. Zero or several stable ReplicaSets make rollback fail closed.
