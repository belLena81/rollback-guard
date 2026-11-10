# Rollback Guardian Technical Specification

## Purpose

Rollback Guardian demonstrates how autonomous agents can safely interact with infrastructure. It is intentionally scoped to one workflow: an elevated HTTP error rate after a Kubernetes deployment, followed by evidence collection, diagnosis, rollback recommendation, approval, execution, and recovery verification.

## In Scope

- Incident creation and lifecycle tracking.
- A2A delegation from Orchestrator to Investigator.
- MCP-backed Kubernetes and observability tools.
- Rule-based, LLM-based, and hybrid diagnosis strategies.
- Rollback proposal validation, policy evaluation, approval binding, and execution.
- Append-only audit timeline.
- Local Docker Compose and `kind` Kubernetes development paths.
- PostgreSQL persistence.

## Out of Scope

- General incident management.
- Fully autonomous infrastructure mutation.
- Multi-tenant or multi-cluster production operation.
- Broad shell, `kubectl`, arbitrary YAML, or delete tools.
- Notification integrations, HA/failover, and production secrets management for v1.

## Components

- Orchestrator: owns incident state, public REST API, validation, policy, approval, execution coordination, audit timeline.
- Investigator Agent: A2A-addressable agent that collects normalized evidence and returns evidence-linked diagnosis.
- Kubernetes MCP Server: one binary deployed twice with separate identities. The read-only catalog is `get_deployment`, `get_replica_sets`, `list_pods`, `get_pod_events`, `get_rollout_status`, `watch_rollout`. The rollback catalog is `prepare_rollback`, `execute_approved_rollback`.
- Observability MCP Server: `get_http_error_rate`, `compare_error_rate_windows`, `get_request_latency`, `get_pod_cpu_usage`, `get_pod_memory_usage`, `evaluate_recovery`.

These catalogs are exhaustive. Clients verify the exact catalog at startup and fail closed on a missing or additional tool.
- Demo API: Kubernetes workload with healthy and deliberately broken image tags.

## Domain Model

Core entities:

- `Incident`: cluster, namespace, deployment, service, symptom, status, version, timestamps.
- `Symptom`: type, observed value, threshold, start time.
- `Evidence`: typed source records linked to an incident.
- `InvestigationConclusion`: hypothesis, confidence, evidence references, recommended action, diagnosis strategy and provenance.
- `PolicyDecision`: allowed flag and the per-rule results that produced it.
- `RollbackProposal`: action ID, proposal hash, current revision, target revision, expiration.
- `Approval`: actor, action ID, proposal hash, decision, expiration.
- `RollbackExecution`: idempotency key, from/to revision, outcome.
- `RecoveryVerification`: rollout and metric recovery result.
- `Actor`: typed identity from Phase 1 onward, even before real auth exists.

Incident statuses: `created`, `investigating`, `awaiting_approval`, `approved`, `executing`, `verifying`, and the terminal statuses `resolved`, `canceled`, `declined` (policy refused a proposal), `rejected` (human rejected), `expired`, `failed`.

The incident state machine must reject illegal transitions with typed domain errors. At most one non-terminal incident may exist per deployment target.

## REST API

Base path: `/v1`.

- `POST /v1/incidents`: create an incident. Returns `202 Accepted` with a `Location` header; investigation runs asynchronously and its progress is visible on the incident resource.
- `GET /v1/incidents/{incidentId}`: return incident summary, status, conclusion, proposal, approval, and latest execution result.
- `GET /v1/incidents/{incidentId}/events`: return ordered audit events.
- `POST /v1/incidents/{incidentId}/approve`: approve an exact action/proposal hash.
- `POST /v1/incidents/{incidentId}/reject`: reject an action with reason.
- `POST /v1/incidents/{incidentId}/cancel`: cancel a non-terminal incident.
- `GET /health/live`, `GET /health/ready`, `GET /metrics`: operational endpoints.

Request bodies are size-limited and decoded strictly (unknown fields rejected). The contract is `api/openapi.yaml`.

Until Phase 7, there is no authentication: a middleware injects a fixed local human `Actor`, and all services must stay on a trusted network.

## Persistence

PostgreSQL is the canonical v1 persistence mechanism. Required tables:

- `incidents`
- `incident_evidence`
- `investigation_results`, `investigation_result_evidence_refs`
- `policy_decisions`
- `action_proposals`
- `approvals`
- `executions`
- `recovery_verifications`
- `operation_events`
- `idempotency_keys`
- `rollback_mutations` (the rollback MCP's own idempotency ledger)
- `investigation_dispatches`, `a2a_tasks`, `a2a_investigation_requests` (durable A2A delegation)

Every state mutation and corresponding audit event must be persisted in the same database transaction. `operation_events` is append-only at the application level.

## Security Requirements

- No generic shell, generic `kubectl`, arbitrary patch, arbitrary delete, or arbitrary YAML MCP tools.
- Read-only and rollback MCP capabilities use separate Kubernetes identities.
- No cluster-admin permissions.
- Namespace and deployment allow-lists are enforced.
- Rollback execution requires non-expired approval, matching proposal hash, matching deployment identity, current-revision validation, idempotency key, and policy pass.
- Secrets and credentials are never logged, returned through tool results, or included in A2A messages.

## Observability

All services expose health endpoints and structured logs with correlation fields such as incident ID, operation ID, task ID, trace ID, and idempotency key. Later phases add Prometheus metrics and OpenTelemetry traces across REST, A2A, MCP, and Kubernetes calls.

## Package Rules

1. `internal/domain` imports no infrastructure packages.
2. Application services depend on interfaces in `internal/ports`.
3. Adapters implement ports.
4. A2A and MCP SDK types remain inside their adapter packages.
5. Avoid generic `utils`, `common`, or `helpers` packages.
6. Interfaces are small and consumer-owned.
7. Constructors validate dependencies.
8. No global mutable state.
9. Every external operation accepts `context.Context`.

## Acceptance Criteria

- Incident can be created through REST.
- Orchestrator delegates investigation through A2A.
- Investigator collects evidence through MCP.
- Structured diagnosis is returned and validated.
- Rollback proposal is generated.
- Human approval is required before mutation.
- Approval is bound to immutable proposal hash.
- Kubernetes MCP executes the rollback.
- Rollout completion and service recovery are verified.
- Incident reaches `resolved`.
- Audit timeline contains every major action.
- Domain logic is independent of MCP and A2A SDKs.
- Kubernetes, LLM, database, and protocol access are behind ports/adapters.
- Security invariants above are independently verifiable.
