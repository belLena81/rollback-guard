# Phase 6 Telemetry

Phase 6 task 3 adds Prometheus metrics, correlated structured logs, and OpenTelemetry tracing to the four Rollback Guardian services. All of it lives in `internal/platform/telemetry`. Prometheus and OTel SDK types do not leave that package; `internal/domain` imports neither.

## Design

- **One provider per process.** Each `cmd/*/main.go` builds a `telemetry.Provider` from `config.TelemetryConfig`. The provider owns a private Prometheus registry (plus the Go and process collectors) and an OTel `TracerProvider`. Nothing registers globally: no `prometheus.DefaultRegisterer`, no `otel.SetTracerProvider`.
- **Application code needs only two port-level additions** (`internal/ports/telemetry.go`):
  - `ports.Tracer`: `TraceCarrier`/`StartSpan`, with a `NoopTracer` default. The application uses it only to persist and resume trace context.
  - `ports.Correlation`: a context value that the log handler reads.
- **Metrics come from decorators at the composition root:**
  - `ObserveUnitOfWork` counts what a transaction commits: audit events and dispatch rows. Rolled-back work is never counted.
  - `ObserveGateway` covers A2A calls.
  - `ObserveDiagnoser` covers diagnosis strategy and fallback.
  - `mcp.ObserveCalls` covers MCP client calls. `mcp.WithToolObserver` is server middleware for MCP tool handling.
  - `InstrumentServer`/`Route` cover HTTP servers.

## Metric catalogue

Every label value comes from a closed set. Incident IDs, idempotency keys, proposal hashes, task IDs, raw paths, and free text are never labels (`TestMetricCatalogue`). Unknown MCP tool names become `unknown`, and unknown HTTP methods become `OTHER`.

| Metric | Type | Labels | Exposed by |
| --- | --- | --- | --- |
| `rollback_guardian_http_server_requests_total` | counter | `method`, `route` (pattern), `code` | all |
| `rollback_guardian_http_server_request_duration_seconds` | histogram | `method`, `route`, `code` | all |
| `rollback_guardian_incidents_total` | counter | (none). Incidents created | orchestrator |
| `rollback_guardian_incidents_terminal_total` | counter | `status`: resolved, canceled, declined, rejected, expired, failed | orchestrator |
| `rollback_guardian_approval_decisions_total` | counter | `decision`: approved, rejected | orchestrator |
| `rollback_guardian_investigation_dispatches_total` | counter | `outcome`: completed, failed, canceled; `reason`: none, submission_failed, investigation_failed, investigation_timeout, invalid_investigation_result, other | orchestrator |
| `rollback_guardian_investigation_duration_seconds` | histogram | `outcome` (dispatch creation to terminal state) | orchestrator |
| `rollback_guardian_a2a_calls_total` | counter | `operation`: submit, watch, get, cancel; `outcome`: ok, error, timeout, canceled | orchestrator |
| `rollback_guardian_a2a_call_duration_seconds` | histogram | `operation`, `outcome` | orchestrator |
| `rollback_guardian_mcp_calls_total` | counter | `server`: kubernetes-readonly, kubernetes-rollback, observability; `tool`; `outcome`: ok, tool_error, error, timeout, canceled | orchestrator, investigator |
| `rollback_guardian_mcp_call_duration_seconds` | histogram | `server`, `tool`, `outcome` | orchestrator, investigator |
| `rollback_guardian_mcp_tool_requests_total` | counter | `server`, `tool`, `outcome` | MCP servers |
| `rollback_guardian_mcp_tool_request_duration_seconds` | histogram | `server`, `tool`, `outcome` | MCP servers |
| `rollback_guardian_diagnoses_total` | counter | `requested_strategy`, `effective_strategy` (rules, llm, hybrid, error), `fallback_reason` (none, provider_error, provider_timeout, provider_refusal, invalid_output) | investigator |
| `rollback_guardian_rollback_executions_total` | counter | `outcome`: succeeded, failed | orchestrator |
| `rollback_guardian_recovery_verifications_total` | counter | `result`: passed, failed | orchestrator |

Health and `/metrics` requests are counted but not traced or logged. On the orchestrator, only registered routes are instrumented. A catch-all route would turn the mux's 405 responses into 404s.

The demo workload's `rollback_guardian_http_requests_total` (scraped from demo-api, labelled by `cluster`/`namespace`/`deployment`/`service`) is a different metric. The services use `rollback_guardian_http_server_*`, so the observability MCP's error-rate query cannot select service traffic.

## Structured logs

Every service logs JSON through `telemetry.NewLogger`. The handler adds `incident_id`, `operation_id`, `task_id`, `idempotency_key` (from `ports.Correlation` in the context), and `trace_id`/`span_id` (from the active span, or from a persisted carrier) to every `*Context` log call. Call sites never add these fields by hand. Values that are not identifier-shaped (`[A-Za-z0-9._:-]`, ≤128 chars) are dropped, so a remote caller cannot inject free text through A2A metadata or MCP arguments.

Correlation is set by:

- **REST:** the `{incidentId}` path value, plus the created incident and its dispatch on `POST /v1/incidents`, plus the execution idempotency key on approve.
- **Coordinator:** the claimed dispatch (incident, operation, idempotency key, trace) and the task ID once submitted. `ReportError` receives that context.
- **A2A executor:** the decoded request and correlation metadata.
- **MCP server middleware:** only the `idempotencyKey` argument.

Never logged: request or response bodies, headers (including `Authorization` and `traceparent`), query strings, DSNs, LLM prompts, provider bodies, and diagnosis hypotheses. Span errors record only a bounded outcome (`error`, `timeout`, `canceled`), never the error text.

## Trace propagation

Only the W3C `traceparent` header is propagated (no baggage).

1. **REST:** `Route` extracts any incoming `traceparent` and starts the server span `POST /v1/incidents`.
2. **REST to coordinator (asynchronous):** `CreateIncidentAsync` stores `Tracer.TraceCarrier(ctx)` (the request span's `traceparent`) in the existing `InvestigationDispatch.TraceID` column. The coordinator later resumes it with `StartSpan("investigation.dispatch", dispatch.TraceID)`, a child of the REST span. Dispatches written before this change hold `trace-…` IDs; those start a new trace.
3. **A2A:** the orchestrator's A2A HTTP client transport injects `traceparent` on every Agent Card, send, get, subscribe, and cancel request. The current carrier also travels in the existing `InvestigationSubmission.TraceID` correlation metadata. The Investigator's server span extracts the header. The executor starts `investigation.execute` from the context span, or from `InvestigationRequest.TraceID` if the task runs detached.
4. **MCP:** the MCP client's HTTP transport injects `traceparent` on each JSON-RPC POST (the SDK builds requests from the call context). On the server, `WithToolObserver` middleware reads the header from `RequestExtra.Header` and starts `mcp tool <name>`.
5. **Kubernetes and Prometheus:** the Kubernetes MCP wraps the client-go transport (`rest.Config.Wrap`) and the observability MCP wraps the Prometheus API round tripper, so API server and PromQL requests become client spans.

The LLM provider client is intentionally not traced: its requests carry the API key and prompt. PostgreSQL calls are not traced.

## Configuration

These settings apply to all four services and are validated in `internal/platform/config` (`TestTelemetrySettingsValidation`).

| Variable | Default | Notes |
| --- | --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | empty (export disabled) | OTLP/HTTP base URL, for example `http://127.0.0.1:4318`. Spans go to `<endpoint>/v1/traces`. `http` or `https` only. Credentials, query, and fragment are rejected. When unset, spans are still created (so logs and persisted carriers have real trace IDs) but are dropped. |
| `OTEL_SERVICE_NAME` | `orchestrator`, `investigator`, `kubernetes-mcp-readonly`/`kubernetes-mcp-rollback`, `observability-mcp` | `service.name` resource attribute. |
| `OTEL_TRACES_SAMPLE_RATIO` | `1` | Parent-based ratio sampler, between `0` and `1`. |
| `TELEMETRY_SHUTDOWN_TIMEOUT` | `5s` | Upper bound for flushing spans at shutdown (≤30s), even if the collector hangs. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

The OTLP exporter also honours the standard `OTEL_EXPORTER_OTLP_HEADERS`/`OTEL_EXPORTER_OTLP_TRACES_*` variables. They are never logged.

## Verification

```bash
go test ./internal/platform/telemetry/... ./internal/ports/... ./internal/platform/config/...
make phase6-telemetry     # in-process REST -> coordinator -> A2A -> Investigator -> MCP trace, headers, logs, /metrics
make contract-test        # /metrics OpenAPI shape, MCP catalogs unchanged by telemetry options
curl -s localhost:8080/metrics | grep '^rollback_guardian_'
```
