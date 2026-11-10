# Phase 6: Retry and Circuit-Breaker Decorators

Transient failures of external systems are absorbed by decorators in `internal/platform/resilience`, applied only in the composition roots (`cmd/*/main.go`). No port signature changed. Application code does not know the decorators exist.

## What is decorated

| Process | Port / seam | Dependency label | Retry | Breaker |
|---|---|---|---|---|
| orchestrator, investigator | `ports.DeploymentInspector` (MCP) | `kubernetes_readonly_mcp` | yes | yes |
| orchestrator, investigator | `ports.MetricsPort` (MCP) | `observability_mcp` | yes | yes |
| orchestrator | `ports.InvestigationGateway.Submit/Get/Cancel` (A2A) | `a2a_investigator` | yes | yes |
| orchestrator | `ports.InvestigationGateway.Watch` | `a2a_investigator` | no (the coordinator re-watches under its lease and attempt budget) | yes |
| orchestrator | `ports.RollbackExecutor` | `kubernetes_rollback_mcp` | only the not-sent rule below | no |
| investigator | `ports.Reasoner` (LLM) | `llm` | yes (`RESILIENCE_LLM_RETRY_MAX_ATTEMPTS`) | yes |
| observability-mcp | `prometheus.QueryAPI` | `prometheus` | yes | yes |
| kubernetes-mcp | read methods of `kubernetesserver.Backend` | `kubernetes_api` | yes | yes |

Each dependency has exactly one breaker per process, shared by every port that calls it. Retries sit outside the telemetry decorators, so each attempt is a counted call.

## What counts as transient

`resilience.IsTransient` is the single classifier. Transient: `context.DeadlineExceeded`, connection and network errors (`*net.OpError`, EOF, closed connection, net timeouts), `ports.ErrReasonerUnavailable`, MCP `dependency_unavailable` (and `timeout`), Prometheus `server_error`/`timeout`/`bad_response`, and Kubernetes server-timeout, timeout, too-many-requests, service-unavailable and internal-error responses. Adapters that own an SDK error flag it through a `Transient() bool` method or `resilience.MarkTransient`, so the package imports no SDK.

Never transient, so never retried and never counted against a breaker: validation, not-found, policy, approval-mismatch, expiry, idempotency and version conflicts, permission errors, `ErrReasonerRefused`, `ErrReasonerInvalidResponse`, `ErrRollbackOutcomeUnknown`, `context.Canceled`, an open breaker, and any unclassified error. These take precedence over a transient cause in the same chain.

## Retry behavior

Attempts are bounded (`RESILIENCE_RETRY_MAX_ATTEMPTS`, counting the first call). The wait before retry *n* is a uniform draw from `[0, min(base·2^(n-1), max))` ("full jitter"). Retrying stops when the caller's context has ended, and a backoff is never started if it would end at or after the context deadline. Time, sleep and jitter are injected (`resilience.Timing`), so the resilience unit tests do not wait. `exhausted` is counted only when the last allowed attempt fails after at least one retry; a deadline cut-off ends retrying without it.

LLM timeouts are not retried: the diagnoser gives each model call a context with `LLM_TIMEOUT`, so a provider timeout leaves the caller's context expired and retrying stops. Retries therefore cover fast failures such as 5xx and connection errors; a timeout counts toward the breaker only while the caller's context is still live.

## Circuit breaker

Closed → open after `RESILIENCE_BREAKER_FAILURE_THRESHOLD` consecutive transient failures. While open, calls fail at once with `*resilience.CircuitOpenError` (matches `resilience.ErrCircuitOpen`) and the dependency is not touched. After `RESILIENCE_BREAKER_OPEN_TIMEOUT` one half-open probe is admitted: success closes the breaker, a transient failure reopens it, and other callers keep failing fast while the probe runs. A non-transient error means the dependency answered, so it resets the streak. Only the probe can close or reopen a non-closed breaker: a call admitted while closed that finishes late is ignored. A caller's own cancellation or deadline is neutral. A probe that is cancelled frees the slot.

LLM: an open breaker is returned as `ports.ErrReasonerUnavailable` wrapping the `CircuitOpenError`. The LLM and hybrid diagnosers already fall back to the rule diagnoser on that error (`fallback_reason=provider_error`), so rules stay in charge. `cmd/investigator/resilience_test.go` proves it against a failing provider.

## Mutation rule

`RollbackExecutor.ExecuteApprovedRollback` (which includes `prepare_rollback`) is repeated only when both hold:

1. `resilience.RequestNotSent(err)`: the TCP dial failed, so the request provably never reached the rollback server. A timeout, a reset, a tool error or any failure after the connection was made does not qualify.
2. The error is not `ports.ErrRollbackOutcomeUnknown`, which the adapter returns on the ambiguous paths it detects. Condition 1 is the real safeguard: a failure after the request was written is never a dial failure, so it is never repeated here. `dial_failure_test.go` pins both directions (refused connection: not sent; server received the call then dropped it: not "not sent").

The MCP adapter classifies every failed `execute_approved_rollback` call (`mutationFailure` in `internal/adapters/mcp/rollback_executor.go`), failing safe:

| Failure | Adapter result |
|---|---|
| Dial refused or failed (`resilience.RequestNotSent`) | returned unchanged; the decorator may repeat the whole call once |
| Definitive server rejection: validation, target forbidden, not found, revision drift, approval mismatch, policy denied, expired, idempotency conflict | returned unchanged; the application records a failed execution |
| Anything else: EOF, connection reset, timeout or cancellation, HTTP 5xx, lost session (404), JSON-RPC error, undecodable or mismatched response, a tool error without a structured result, `internal`, `dependency_unavailable`, `timeout`, `canceled` or unknown tool error codes | reconcile: re-inspect the deployment; if it is at the target revision, replay the call with the same key (the ledger answers without a second mutation) and watch the rollout; otherwise, or if the replay fails, `ErrRollbackOutcomeUnknown` |

Before the mutation, the adapter inspects the Deployment and calls `prepare_rollback`. If that inspection failed, "not applied" was never established, so any `prepare_rollback` failure other than a dial failure is also `ErrRollbackOutcomeUnknown` (a revision-drift answer may mean an earlier attempt already applied the rollback). A caller that has already cancelled stops before `prepare_rollback`, with a plain cancellation.

The rejection row is only sound if the server never sends a rejection code after it mutated. The Kubernetes service therefore wraps every ledger failure that happens once the Deployment is at the target revision (`Complete` after the patch, or on the already-applied and reconcile paths) as `kubernetes.ErrMutationNotRecorded`, which hides the cause's domain identity; the tool reports it as `internal`.

`ErrRollbackOutcomeUnknown` leaves the orchestrator's execution attempt pending and the incident `executing`; retrying the same approval reuses the same key and reconciles it. The REST API answers it with `503`, error `rollback_outcome_unknown`, and `Retry-After` (`api/openapi.yaml`), never as a plain `500`. `rollback_transport_failure_test.go` injects each post-send fault into a real HTTP MCP exchange, both when the server applied the rollback and when it did not.

The repeat reuses the same approved rollback and idempotency key, which the port contract and the server's `rollback_mutations` ledger treat as one logical request. Attempts are capped at 2 regardless of the general setting. Everything else, including every ambiguous failure, is returned unchanged, so the application surfaces `ErrRollbackOutcomeUnknown` exactly as before. There is no breaker on this path: a breaker rejection would turn a human-approved rollback into a recorded failed execution without trying it. `mcp/dial_failure_test.go` pins that a refused connection stays recognizable through the MCP SDK; if an SDK upgrade loses that, the retry silently stops firing (it never fires wrongly).

On the Kubernetes MCP server, `PrepareRollback`, `ExecuteApprovedRollback` and `WatchRollout` pass through the read decorator untouched.

Recovery verification reads go through the decorated inspector and metrics ports, so they retry like any read.

## Configuration

Shared by every process; an invalid value stops the process at start-up. Defaults and bounds are named constants in `internal/platform/config/resilience.go`.

| Variable | Default | Bounds | Meaning |
|---|---|---|---|
| `RESILIENCE_RETRY_MAX_ATTEMPTS` | `3` | 1..10 | Total calls per read, including the first. `1` disables retries. |
| `RESILIENCE_LLM_RETRY_MAX_ATTEMPTS` | `2` | 1..10 | Same, for LLM calls (slow and billed). |
| `RESILIENCE_RETRY_BASE_DELAY` | `200ms` | 10ms..1m | Backoff ceiling before the first retry; doubles each retry. |
| `RESILIENCE_RETRY_MAX_DELAY` | `2s` (or the base delay if larger) | 10ms..1m, ≥ base | Backoff ceiling cap. |
| `RESILIENCE_BREAKER_FAILURE_THRESHOLD` | `5` | 1..100 | Consecutive transient failures that open a breaker. |
| `RESILIENCE_BREAKER_OPEN_TIMEOUT` | `30s` | 1s..10m | Time an open breaker rejects calls before one probe. |

## Metrics

Both use the per-process registry (see `docs/phase6-telemetry.md`); labels come from closed sets.

| Metric | Type | Labels |
|---|---|---|
| `rollback_guardian_retries_total` | counter | `dependency`, `outcome`: `retried` (a failed attempt followed by another), `exhausted` (failed on the last allowed attempt after at least one retry) |
| `rollback_guardian_circuit_breaker_transitions_total` | counter | `dependency`, `state` entered: `open`, `half_open`, `closed` |

Breaker transitions are also logged at warn level with the dependency and state.

## Verification

```bash
go test -race ./internal/platform/resilience/ ./internal/adapters/prometheus/ ./internal/adapters/mcp/... ./cmd/investigator/
make lint test-race contract-test
```
