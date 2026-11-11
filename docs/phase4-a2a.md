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
AUTH_MODE=local go run ./cmd/orchestrator   # no login; the default AUTH_MODE=github needs docs/phase7-auth.md
```

Check discovery and create an asynchronous incident:

```bash
curl -sS http://127.0.0.1:8094/.well-known/agent-card.json
curl -sS -X POST http://127.0.0.1:8080/v1/incidents \
  -H 'Content-Type: application/json' \
  -d '{"target":{"cluster":"local","namespace":"rollback-demo","deployment":"demo-api","service":"demo-api"},"symptom":{"kind":"http_error_rate","observed":0.35,"threshold":0.10,"unit":"ratio","startedAt":"2026-08-05T10:00:00Z"}}'
```

Poll the URL returned in the `Location` response header. `investigation.state` progresses from `queued` through A2A task states to `completed`, `failed`, or `canceled`.

The Investigator never pauses a task for input or authentication. An investigation task that enters `input-required` or `auth-required` fails at once as `invalid_investigation_result`, its remote task is canceled, and it is not retried; approval stays an Orchestrator REST command (ADR-01).

## Runtime Controls

| Variable | Default | Purpose |
| --- | --- | --- |
| `INVESTIGATOR_URL` | `http://127.0.0.1:8094` | Orchestrator discovery base URL |
| `INVESTIGATOR_PUBLIC_URL` | `http://127.0.0.1:8094` | URL advertised by the Agent Card |
| `HTTP_ADDR` | `127.0.0.1:8094` (investigator), `127.0.0.1:8080` (orchestrator) | Listen address |
| `A2A_CONNECT_TIMEOUT` | value of `MCP_CONNECT_TIMEOUT` (`5s`) | Agent Card discovery and client setup at orchestrator startup (≤30s). It used to share `MCP_CONNECT_TIMEOUT`, which stays its default |
| `A2A_TASK_TIMEOUT` | `45s` | Total investigation deadline (≤2m) |
| `A2A_CANCEL_TIMEOUT` | `5s` | Remote cancellation and cleanup deadline |
| `A2A_POLL_INTERVAL` | `250ms` | Durable coordinator polling interval |
| `A2A_LEASE_TTL` | task plus cancellation timeout | Dispatch recovery lease |
| `A2A_WORKER_ID` | `orchestrator-1` | Coordinator lease owner |
| `A2A_MAX_ATTEMPTS` | `3` | Maximum ambiguous or transient submission/watch attempts |

The Investigator deduplicates requests by idempotency key. Both intervals of that record follow `A2A_TASK_TIMEOUT`: a `working` record older than the task timeout plus 10s (5s for the terminal write, 5s of margin for clock skew) belongs to an owner that is gone and is reclaimed by the next request (55s by default; this was a fixed 2m), and a duplicate request polls the owner's record every hundredth of the task timeout, between 10ms and 100ms (100ms by default, as before). Investigator readiness is bounded by `DB_PING_TIMEOUT` (default `2s`).

Authentication is intentionally deferred for Phase 4. Every service, including the MCP servers, binds loopback by default (`HTTP_ADDR`) and must remain network-isolated until the authentication phase adds an A2A credential mechanism. Set `HTTP_ADDR` explicitly only where a service has to be reachable from another network namespace, as the service image does (`deploy/docker/service.Dockerfile`).
