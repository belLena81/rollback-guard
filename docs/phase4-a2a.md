# Phase 4 A2A Runtime

Phase 4 moves rollout investigation behind the official A2A HTTP+JSON REST binding. The Orchestrator creates an incident and durable dispatch record synchronously, returns `202 Accepted`, and a coordinator worker submits and watches the remote task. The Investigator owns MCP evidence collection and diagnosis. It never approves or executes a rollback.

## Protocol Surface

- Discovery: `GET /.well-known/agent-card.json`
- A2A REST endpoint: `/a2a`
- Health: `GET /health/live` and `GET /health/ready`
- Required extension: `https://rollback-guardian.dev/a2a/extensions/investigation/v1`
- Input and output: exactly one `application/json` DataPart using schema version `1`
- Result: exactly one artifact named `investigation-result`

The adapter rejects unknown fields, unsupported schema versions, missing correlation metadata, non-DataPart content, and unexpected artifacts. A2A SDK types remain under `internal/adapters/a2a`.

## Local Verification

Run the default gates:

```bash
GOCACHE=/tmp/rollback-guard-go-cache make test
GOCACHE=/tmp/rollback-guard-go-cache make vet
GOCACHE=/tmp/rollback-guard-go-cache make lint
```

Run the hermetic A2A/PostgreSQL test. It starts and removes a PostgreSQL 16 container and uses a loopback HTTP server; it does not require the application Compose stack:

```bash
GOCACHE=/tmp/rollback-guard-go-cache make phase4-integration
```

For manual execution, apply migrations `000001` through `000003`, start the Phase 3 read-only Kubernetes and observability MCP servers, then start the Investigator before the Orchestrator:

```bash
DATABASE_URL='postgres://rollback_guard:rollback_guard@127.0.0.1:5432/rollback_guard?sslmode=disable' \
HTTP_ADDR='127.0.0.1:8094' \
INVESTIGATOR_PUBLIC_URL='http://127.0.0.1:8094' \
go run ./cmd/investigator

DATABASE_URL='postgres://rollback_guard:rollback_guard@127.0.0.1:5432/rollback_guard?sslmode=disable' \
INVESTIGATOR_URL='http://127.0.0.1:8094' \
go run ./cmd/orchestrator
```

Check discovery and create an asynchronous incident:

```bash
curl -sS http://127.0.0.1:8094/.well-known/agent-card.json
curl -sS -X POST http://127.0.0.1:8080/v1/incidents \
  -H 'Content-Type: application/json' \
  -d '{"target":{"cluster":"local","namespace":"rollback-demo","deployment":"demo-api","service":"demo-api"},"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-05T10:00:00Z"}}'
```

Poll the URL returned in the `Location` response header. `investigation.state` progresses from `queued` through A2A task states to `completed`, `failed`, or `canceled`.

## Runtime Controls

| Variable | Default | Purpose |
| --- | --- | --- |
| `INVESTIGATOR_URL` | `http://127.0.0.1:8094` | Orchestrator discovery base URL |
| `INVESTIGATOR_PUBLIC_URL` | `http://127.0.0.1:8094` | URL advertised by the Agent Card |
| `A2A_TASK_TIMEOUT` | `45s` | Total investigation deadline |
| `A2A_CANCEL_TIMEOUT` | `5s` | Remote cancellation and cleanup deadline |
| `A2A_POLL_INTERVAL` | `250ms` | Durable coordinator polling interval |
| `A2A_LEASE_TTL` | task plus cancellation timeout | Dispatch recovery lease |
| `A2A_WORKER_ID` | `orchestrator-1` | Coordinator lease owner |
| `A2A_MAX_ATTEMPTS` | `3` | Maximum ambiguous or transient submission/watch attempts |

Authentication is intentionally deferred for Phase 4. Both services default to loopback endpoints and must remain network-isolated until the authentication phase adds an A2A credential mechanism.
