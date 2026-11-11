# Phase 3 MCP Runtime

Phase 3 replaces the orchestrator's fake infrastructure adapter with three independent Streamable HTTP MCP connections:

- Kubernetes read-only MCP on `127.0.0.1:8091`.
- Kubernetes rollback MCP on `127.0.0.1:8092`.
- Observability MCP on `127.0.0.1:8093`.

The orchestrator itself listens on `127.0.0.1:8080`. Every process binds loopback by default because only the orchestrator authenticates (`AUTH_MODE=github`, `docs/phase7-auth.md`); the MCP servers and the investigator are network-trusted. Set `HTTP_ADDR` only where a process must be reachable from another network namespace (the service image does, see `deploy/docker/service.Dockerfile`), and keep that port on a trusted network.

Run the Kubernetes binary twice with different service-account credentials and `KUBERNETES_MCP_MODE=readonly` or `rollback`. Apply `deploy/rbac/mcp.yaml` to create the separate namespace-scoped identities. Use `make deploy` (`kubectl apply -k deploy/kubernetes`), which creates the `rollback-demo` namespace first (see `docs/phase6-kind.md`). The rollback identity can only get and patch `rollback-demo/demo-api`; it cannot update arbitrary resources, read secrets, delete objects, or execute in pods.

The previous stable ReplicaSet must carry this annotation:

```yaml
rollback-guardian.io/stable: "true"
```

Revisions are deterministic SHA-256 digests of normalized Pod templates, rather than mutable image tags or ReplicaSet names. A rollback patch replaces only `spec.template` and includes the observed Deployment `resourceVersion` to reject concurrent rollout drift.

The mutating tool accepts only a proposal hash and idempotency key. Before any Kubernetes access, the rollback MCP loads the matching pending execution, canonical proposal, allowed policy decision, and approved human approval from PostgreSQL. Its separate `rollback_mutations` ledger preserves the key-to-proposal binding and exact mutation result across restarts and replicas. The orchestrator remains responsible for completing its own execution/idempotency record and audit transition.

Concurrent calls for one idempotency key are serialized by a PostgreSQL advisory lock taken in `RollbackMutationGuard.Begin`. Each pending attempt receives its own random lease (`ports.RollbackMutationLease`), and `Complete` and `Release` act only on the lock acquired under that lease. A late release from an earlier attempt therefore cannot unlock a replay of the same key that holds the lock now; a late `Complete` returns `ports.ErrRollbackMutationLeaseNotHeld` and writes nothing. A ledger failure after the Deployment reached the target revision is returned as `ErrMutationNotRecorded` and reported as `internal`, never as a rejection code, so the client reconciles it.

On the orchestrator side, the MCP rollback adapter treats every failure of `execute_approved_rollback` after the request may have reached the server as an unknown outcome and reconciles it (see `docs/phase6-resilience.md`, "Mutation rule").

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
| `RECOVERY_TIMEOUT` | `METRICS_WINDOW` + 30s, at most `7m` |
| `RECOVERY_POLL_INTERVAL` | `5s` (1s..1m) |

Recovery verification runs right after the rollout watch, when the metrics window still holds the errors from before the rollback. It therefore checks once and then every `RECOVERY_POLL_INTERVAL` until recovery passes or `RECOVERY_TIMEOUT` is used up, and records the first passing or the last failing result. A check that cannot read the deployment or the metric (for example no fresh series right after the pod switch) counts as neither; if the last check cannot read either, the error is returned and the incident stays `verifying` until the approval is retried. `RECOVERY_TIMEOUT` is capped so that an approve request (rollout watch plus recovery wait) stays inside the 10m server write timeout.

Both Kubernetes processes use `ALLOWED_TARGETS` in `cluster/namespace/deployment/service` form, `CLUSTER_ID`, optional `KUBECONFIG`, `MCP_REQUEST_TIMEOUT`, and `MCP_SESSION_TIMEOUT`. Their `HTTP_ADDR` defaults to `127.0.0.1:8091` in readonly mode and `127.0.0.1:8092` in rollback mode. Rollback mode additionally requires `DATABASE_URL` and `AUTH_MODE` (`github` or `local`, the same value as the orchestrator; there is no default, an unset or unknown value refuses to start), and accepts `DB_MAX_CONNS` (default `4`), `DB_PING_TIMEOUT` (default `2s`) and, in local mode, `LOCAL_ACTOR_ID` (default `local-admin`, as in the orchestrator); readonly mode does not connect to PostgreSQL and ignores `AUTH_MODE`.

**Approver re-check at execution.** In the same transaction in which `execute_approved_rollback` re-loads the pending execution, proposal, policy decision and approval, the rollback MCP checks the approval's actor. `github` mode: the actor must be `github:<id>` and that user must currently be enabled with a role that `domain.RolePolicy` allows to approve a rollback (administrator), read live from `users`. `local` mode: there are no users rows, so only an approval recorded by exactly the configured local actor is executable. Otherwise the tool returns the fixed error code `approver_not_authorized` (the orchestrator maps it to `domain.ApproverNotAuthorizedError`, a definitive rejection, HTTP `403 forbidden`), writes no `rollback_mutations` row and does not touch Kubernetes. `local` mode refuses to start with a `github:<id>` `LOCAL_ACTOR_ID`. The check also runs when a pending mutation is resumed. A refusal there is not definitive, because an earlier attempt may have patched before it crashed. The attempt is therefore reconcile-only:
- If the Deployment is already at the target revision, it is recorded as applied.
- Otherwise the attempt is released with `approver_not_authorized` and no patch.

A completed mutation replays from the ledger without the check. A demotion that commits after the check, while the patch is in flight, is not undone: the check is at mutation start. The tool's arguments are unchanged (proposal hash and idempotency key only). The observability process uses `HTTP_ADDR` (default `127.0.0.1:8093`), `PROMETHEUS_URL`, `PROMETHEUS_TIMEOUT`, `METRICS_MAX_SAMPLE_AGE`, the same target allow-list, and metrics window.

Durations outside their bounds are rejected at startup with the allowed range and the offending value or parse error. `METRICS_WINDOW` must be between `30s` and `1h` and `MCP_WATCH_TIMEOUT` between `1s` and `2m`; the same bounds (`internal/domain/limits.go`) are re-checked by the MCP servers and adapters.

`ALLOWED_TARGETS` defaults to `local/rollback-demo/demo-api/demo-api`, matching the namespace and deployment in `deploy/rbac/mcp.yaml`.

Use a dedicated rollback MCP database role. It needs `SELECT` on `incidents`, `action_proposals`, `policy_decisions`, `approvals`, and `idempotency_keys`, plus `SELECT`, `INSERT`, and `UPDATE` on `rollback_mutations`. In `github` mode it also needs the column-level `GRANT SELECT (github_id, role, disabled) ON users` for the approver re-check (the query reads only those columns, never login, email or sessions); `local` mode does not read `users`. It does not need write access to workflow or audit tables. Database URLs and credentials must not be logged.

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
