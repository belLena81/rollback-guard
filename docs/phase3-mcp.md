# Phase 3 MCP Runtime

Phase 3 replaces the orchestrator's fake infrastructure adapter with three independent Streamable HTTP MCP connections:

- Kubernetes read-only MCP on `:8091`.
- Kubernetes rollback MCP on `:8092`.
- Observability MCP on `:8093`.

Run the Kubernetes binary twice with different service-account credentials and `KUBERNETES_MCP_MODE=readonly` or `rollback`. Apply `deploy/rbac/mcp.yaml` to create the separate namespace-scoped identities. The rollback identity can only get and patch `rollback-demo/demo-api`; it cannot update arbitrary resources, read secrets, delete objects, or execute in pods.

The previous stable ReplicaSet must carry this annotation:

```yaml
rollback-guardian.io/stable: "true"
```

Revisions are deterministic SHA-256 digests of normalized Pod templates, rather than mutable image tags or ReplicaSet names. A rollback patch replaces only `spec.template` and includes the observed Deployment `resourceVersion` to reject concurrent rollout drift.

## Configuration

The orchestrator requires `DATABASE_URL` and accepts:

| Variable | Default |
| --- | --- |
| `KUBERNETES_READ_MCP_URL` | `http://127.0.0.1:8091/mcp` |
| `KUBERNETES_ROLLBACK_MCP_URL` | `http://127.0.0.1:8092/mcp` |
| `OBSERVABILITY_MCP_URL` | `http://127.0.0.1:8093/mcp` |
| `MCP_CONNECT_TIMEOUT` | `5s` |
| `MCP_CALL_TIMEOUT` | `10s` |
| `MCP_WATCH_TIMEOUT` | `90s` |
| `METRICS_WINDOW` | `5m` |

Both Kubernetes processes use `ALLOWED_TARGETS` in `cluster/namespace/deployment/service` form, `CLUSTER_ID`, optional `KUBECONFIG`, `MCP_REQUEST_TIMEOUT`, and `MCP_SESSION_TIMEOUT`. The observability process uses `PROMETHEUS_URL`, `PROMETHEUS_TIMEOUT`, `METRICS_MAX_SAMPLE_AGE`, the same target allow-list, and metrics window.

All processes expose `/health/live` and `/health/ready`. The orchestrator is ready only when PostgreSQL and all three MCP sessions respond. It validates each server's exact tool catalog during startup and fails closed if a tool is missing or an unexpected capability appears.

## Verification

```bash
GOCACHE=/tmp/rollback-guard-go-cache make contract-test
GOCACHE=/tmp/rollback-guard-go-cache make phase3-integration
GOCACHE=/tmp/rollback-guard-go-cache ./.codex/checks.sh
```
