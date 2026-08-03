# Rollback Guardian Design Notes

## Differentiators

The project should prioritize three visible trust features:

- Replayable evidence-to-decision timeline.
- Policy that visibly declines unsafe or low-confidence action.
- Verifiable least-privilege Kubernetes boundary.

Keep other features lightweight so these are complete and demonstrable.

## Workflow

1. Incident is created from a deployment regression symptom.
2. Orchestrator persists the incident and delegates investigation through A2A.
3. Investigator gathers deployment, pod, event, log, and metric evidence through MCP.
4. Diagnoser returns a structured, evidence-linked conclusion.
5. Orchestrator validates schema and evidence references.
6. Policy decides whether a rollback proposal may be presented.
7. Human approves or rejects the exact proposal hash.
8. Rollback MCP executes the approved action.
9. Verifier checks rollout and metric recovery.
10. Audit timeline records every step.

## A2A vs MCP

- A2A is for agent-to-agent task delegation, lifecycle state, progress, cancellation, and result delivery.
- MCP is for tool access, capability scoping, and typed infrastructure interactions.

Do not collapse these protocols into one internal REST abstraction; the separation is part of the architecture being demonstrated.

## Layering

Use ports and adapters:

- `internal/domain`: entities, state machine, pure policy/eligibility logic.
- `internal/application`: use cases and orchestration over ports.
- `internal/ports`: small interfaces owned by callers.
- `internal/adapters`: A2A, MCP, Kubernetes, Prometheus, PostgreSQL, LLM, memory.
- `internal/platform`: config, logging, telemetry, HTTP server concerns.

## Timeline Data

Capture enough data to reconstruct decisions:

- Incident and symptom.
- Evidence IDs and normalized evidence summaries.
- Diagnosis strategy, confidence, hypothesis, evidence references.
- Policy rule results.
- Proposal hash and expiration.
- Approval actor and decision.
- Execution and verification result.
- Correlation IDs for REST, A2A, MCP, and Kubernetes operations.

## Local Demo Shape

The demo should be repeatable:

- Deploy healthy `demo-api` image.
- Generate baseline traffic or synthetic metrics.
- Deploy broken image tag.
- Trigger incident.
- Approve rollback.
- Verify healthy revision and audit trail.

Prefer a reliable scripted scenario over broad observability realism.
