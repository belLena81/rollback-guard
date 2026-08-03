# Rollback Guardian Feature Plan

## Problem Statement

Teams are increasingly asked to trust autonomous operational agents, but most demos hide the reasoning and safety boundary. Rollback Guardian demonstrates a narrower, stronger claim: an agent can investigate and recommend, while deterministic controls and human approval remain responsible for mutation.

## Personas

- Platform engineer: wants safe automation, auditability, and least privilege.
- Engineering manager: wants a concise demonstration of agent governance and production architecture.
- Reviewer/interviewer: wants to inspect design judgment, testability, and implementation boundaries.

## Epics

- Investigation and evidence gathering: detect or accept an incident, gather rollout and metric evidence, correlate timing.
- Governed decision-making: generate confidence-scored diagnosis, enforce policy, show policy refusal.
- Controlled remediation: prepare immutable rollback proposal, require approval, execute idempotently.
- Verified recovery: watch rollout, compare metrics, close only after recovery criteria pass.
- Transparency and audit: show replayable evidence-to-decision timeline.
- Operability and trust controls: health, logs, metrics, traces, RBAC checks, CI.

## Must Have

- REST incident flow.
- A2A investigation boundary.
- MCP infrastructure tool boundary.
- Rule-based diagnosis.
- Approval-bound rollback proposal.
- PostgreSQL-backed audit timeline.
- Local `kind` demo with healthy and broken rollout.
- Tests proving state machine, policy, approval binding, idempotency, and tool restrictions.

## Should Have

- Hybrid LLM diagnosis.
- Timeline page or terminal view.
- Policy-decline demo scenario.
- OpenTelemetry trace across the workflow.

## Could Have

- Multiple LLM providers.
- Prometheus-backed metrics instead of synthetic/direct scrape metrics.
- TUI approval prompt.

## Won't Have in v1

- Full incident-management workflows.
- Unapproved autonomous rollback.
- Multi-cluster production support.
- Slack/PagerDuty integration.
- Production-grade secrets platform.

## Demo Milestones

1. Domain and HTTP workflow works against fakes.
2. Real MCP tools execute against a disposable Kubernetes target.
3. A2A service split is complete.
4. LLM/hybrid diagnosis is optional and fallback-safe.
5. Full local `kind` demo and audit timeline are complete.
