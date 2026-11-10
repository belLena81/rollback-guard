# Phase 3 MCP Runtime

Phase 3 replaces the orchestrator's fake infrastructure adapter with three independent Streamable HTTP MCP connections:

- Kubernetes read-only MCP on `127.0.0.1:8091`.
- Kubernetes rollback MCP on `127.0.0.1:8092`.
- Observability MCP on `127.0.0.1:8093`.

The orchestrator itself listens on `127.0.0.1:8080`. Every process binds loopback by default because nothing is authenticated until Phase 7. Set `HTTP_ADDR` only where a process must be reachable from another network namespace (the service image does, see `deploy/docker/service.Dockerfile`), and keep that port on a trusted network.

Run the Kubernetes binary twice with different service-account credentials and `KUBERNETES_MCP_MODE=readonly` or `rollback`. Apply `deploy/rbac/mcp.yaml` to create the separate namespace-scoped identities. Use `make deploy` (`kubectl apply -k deploy/kubernetes`), which creates the `rollback-demo` namespace first (see `docs/phase6-kind.md`). The rollback identity can only get and patch `rollback-demo/demo-api`; it cannot update arbitrary resources, read secrets, delete objects, or execute in pods.

The previous stable ReplicaSet must carry this annotation:

```yaml
rollback-guardian.io/stable: "true"
```

Revisions are deterministic SHA-256 digests of normalized Pod templates, rather than mutable image tags or ReplicaSet names. A rollback patch replaces only `spec.template` and includes the observed Deployment `resourceVersion` to reject concurrent rollout drift.

The mutating tool accepts only a proposal hash and idempotency key. Before any Kubernetes access, the rollback MCP loads the matching pending execution, canonical proposal, allowed policy decision, and approved human approval from PostgreSQL. Its separate `rollback_mutations` ledger preserves the key-to-proposal binding and exact mutation result across restarts and replicas. The orchestrator remains responsible for completing its own execution/idempotency record and audit transition.

## Configuration

The orchestrator requires `DATABASE_URL` and accepts:

| Variable | Default |
| --- | --- |
| `HTTP_ADDR` | `127.0.0.1:8080` |
| `KUBERNETES_READ_MCP_URL` | `http://127.0.0.1:8091/mcp` |
| `KUBERNETES_ROLLBACK_MCP_URL` | `http://127.0.0.1:8092/mcp` |
| `OBSERVABILITY_MCP_URL` | `http://127.0.0.1:8093/mcp` |
| `MCP_CONNECT_TIMEOUT` | `5s` |
| `MCP_CALL_TIMEOUT` | `10s` |
| `MCP_WATCH_TIMEOUT` | `90s` |
| `METRICS_WINDOW` | `5m` |

Both Kubernetes processes use `ALLOWED_TARGETS` in `cluster/namespace/deployment/service` form, `CLUSTER_ID`, optional `KUBECONFIG`, `MCP_REQUEST_TIMEOUT`, and `MCP_SESSION_TIMEOUT`. Their `HTTP_ADDR` defaults to `127.0.0.1:8091` in readonly mode and `127.0.0.1:8092` in rollback mode. Rollback mode additionally requires `DATABASE_URL` and accepts `DB_MAX_CONNS` (default `4`) and `DB_PING_TIMEOUT` (default `2s`); readonly mode does not connect to PostgreSQL. The observability process uses `HTTP_ADDR` (default `127.0.0.1:8093`), `PROMETHEUS_URL`, `PROMETHEUS_TIMEOUT`, `METRICS_MAX_SAMPLE_AGE`, the same target allow-list, and metrics window.

Durations outside their bounds are rejected at startup with the allowed range and the offending value or parse error. `METRICS_WINDOW` must be between `30s` and `1h` and `MCP_WATCH_TIMEOUT` between `1s` and `2m`; the same bounds (`internal/domain/limits.go`) are re-checked by the MCP servers and adapters.

`ALLOWED_TARGETS` defaults to `local/rollback-demo/demo-api/demo-api`, matching the namespace and deployment in `deploy/rbac/mcp.yaml`.

Use a dedicated rollback MCP database role. It needs `SELECT` on `incidents`, `action_proposals`, `policy_decisions`, `approvals`, and `idempotency_keys`, plus `SELECT`, `INSERT`, and `UPDATE` on `rollback_mutations`. It does not need write access to workflow or audit tables. Database URLs and credentials must not be logged.

All processes expose `/health/live` and `/health/ready` through one shared handler (`internal/platform/httpserver`). Success is `200 {"status":"ok"}`; a failed or timed-out readiness check is `503 {"status":"not_ready"}` and never includes the cause. One readiness check is bounded by `DB_PING_TIMEOUT` (orchestrator, investigator, rollback-mode Kubernetes MCP), by `PROMETHEUS_TIMEOUT` (observability MCP), or by the 2s default (readonly Kubernetes MCP).

All processes also share one HTTP server (`httpserver.Serve`): 5s to read request headers, 30s to read a request, a 2m idle timeout, a 10s graceful shutdown, and a 10m write timeout. The write timeout is deliberately far above the longest response (a `watch_rollout` call blocks for up to 2m before it answers, and an approve request waits for that watch), because Go closes the connection without telling the handler when it expires.

Rollback-mode Kubernetes MCP readiness checks both Kubernetes and PostgreSQL; readonly mode checks only Kubernetes. The orchestrator is ready only when PostgreSQL and all three MCP sessions respond. It validates each server's exact tool catalog during startup and fails closed if a tool is missing or an unexpected capability appears.

## Verification

```bash
GOCACHE=/tmp/rollback-guard-go-cache make contract-test
GOCACHE=/tmp/rollback-guard-go-cache make phase3-integration
GOCACHE=/tmp/rollback-guard-go-cache make postgres-integration   # adapter tests; any skipped test fails the target
GOCACHE=/tmp/rollback-guard-go-cache make integration            # every integration-tagged suite
```

`make postgres-integration` needs `ROLLBACK_GUARD_POSTGRES_TEST_DSN` pointing at a database whose name contains `test` (the sandbox sets it). `make lint` and `make vet` also cover the `integration`- and `kind`-tagged test files.
