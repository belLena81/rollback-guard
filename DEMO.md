# Demo Guide

How to run Rollback Guardian locally and show it to others. Reference: [docs/local-demo-env.md](docs/local-demo-env.md), [docs/phase6-kind.md](docs/phase6-kind.md), [docs/phase7-auth.md](docs/phase7-auth.md). The Makefile is the source of truth.

## The story

A bad Kubernetes rollout is detected, the investigator gathers evidence, a rollback is proposed, a human approves the exact proposal hash, a narrow MCP tool executes it, and recovery is verified.

Points to make while demoing:

- The AI only investigates and proposes. It never approves or executes.
- Only an administrator can approve, and the approval is bound to the proposal hash.
- The rollback MCP tool takes only the hash and an idempotency key, and re-checks that the approver is still an active administrator.
- Least privilege is checked: `make rbac-check` shows the read-only account cannot patch deployments.

## Prerequisites

Go, Docker with Compose, `kind`, `kubectl`, `make`, `bash`, `curl`, `jq`, and the golang-migrate CLI (`migrate`). Ollama or an LLM key is optional (only for `llm_based` / `hybrid` diagnosis).

No local installs? `./.devcontainer/sandbox.sh up` gives a container with Docker, kind, Postgres and Prometheus ([.devcontainer/README.md](.devcontainer/README.md)).

## One-time setup (about 5 minutes)

```bash
make kind-create                  # local Kubernetes cluster
make compose-up                   # Postgres + Prometheus
export DATABASE_URL='postgres://rollback_guard:rollback_guard@127.0.0.1:5432/rollback_guard?sslmode=disable'
export PROMETHEUS_URL=http://127.0.0.1:9090
make kind-load deploy             # build and load the demo app, deploy it
make demo-v1                      # healthy v1, marked as the stable version
make services-up                  # build, migrate, start orchestrator, investigator, 3 MCP servers
make services-status              # all should report ready
```

## Live demo

Run `make generate-load` in a second terminal and leave it running.

```bash
make demo-v2-broken          # 1. the bad rollout (error storm)
make create-incident         # 2. incident opened, investigator collects evidence
make approve-latest-action   # 3. human approves the exact proposal hash
make verify                  # 4. rollback executed, error rate recovered, incident resolved
```

Recovery appears about 30 seconds after the rollback (the demo uses a 30s metrics window). The orchestrator API is at `127.0.0.1:8080` (contract: `api/openapi.yaml`). To run it again: `make demo-reset`.

## Variants

| Goal | Commands |
| --- | --- |
| Unattended run, approve path | `make services-down`, then `make e2e` (about a minute) |
| Unattended run, decline path | `make services-down`, then `make e2e-decline` |
| Real login, roles, audit log | `make services-down`, then `make services-up DEMO_AUTH=github` |
| LLM diagnosis | set `DIAGNOSIS_STRATEGY=hybrid` and the `LLM_*` variables before `services-up` ([docs/phase5-ai-diagnosis.md](docs/phase5-ai-diagnosis.md)) |
| Traces | `make compose-up COMPOSE_PROFILES="--profile tracing"`, set `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318` |

With `DEMO_AUTH=github` a fake GitHub provides viewer, operator and administrator users:

- `APPROVE_AS=operator make approve-latest-action` shows the 403: operators cannot approve.
- `GET /v1/auth/audit` (administrator) shows the append-only auth audit log.
- `make auth-login` issues fresh session tokens.

The e2e targets leave running services alone, so run `make services-down` first. `make e2e-test` runs the same flows as a Go test and needs `make services-up DEMO_AUTH=github` first.

## Troubleshooting

- `DATABASE_URL is required`: export it as above (the e2e flows need `make compose-up` too).
- Recovery never completes: recovery needs live traffic, so keep `make generate-load` running.
- Switching auth modes: `make services-down` first.
- Service logs: `.demo/logs`; status: `make services-status`.

## Cleanup

```bash
make services-down compose-down
make kind-delete
```
