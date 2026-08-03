# Rollback Guardian Tech Stack

## Go

Use modern Go with standard library defaults where practical:

- `net/http` and Go 1.22+ method/path mux for HTTP.
- `log/slog` for structured logs.
- Manual constructor injection.
- Table-driven tests.

## Protocols

- A2A: official `github.com/a2aproject/a2a-go` SDK.
- MCP: official `github.com/modelcontextprotocol/go-sdk` where practical.
- JSON schema: prefer typed Go structs and generated/derived schemas over unstructured `map[string]any`.

## Kubernetes and Metrics

- Kubernetes API: `k8s.io/client-go`.
- Local cluster: `kind`.
- Metrics: start with direct/synthetic app metrics for repeatability; add Prometheus when the rest of the workflow is stable.

## Persistence

- PostgreSQL is canonical for v1.
- Use `pgx`/`pgxpool`.
- Use migrations.
- Use `sqlc` if query volume grows enough to justify generated typed access.
- Use Testcontainers for integration tests.

SQLite appeared in earlier source notes as a zero-friction alternative, but it conflicts with the technical specification and development plan. Do not choose SQLite unless a future ADR explicitly supersedes the PostgreSQL decision.

## LLM Providers

Keep model access behind one port. Reasonable implementations:

- Rule-based fallback: required first.
- Groq or OpenRouter: OpenAI-compatible hosted APIs.
- Ollama: offline local demo path.
- Gemini: optional provider-specific path.

The LLM may only produce structured diagnosis data. It may not call tools, approve, execute, or bypass validation.

## Testing

- Unit: pure domain and application logic.
- Contract: REST/OpenAPI, A2A Agent Card/tasks, MCP tool schemas.
- Integration: PostgreSQL, MCP server/client, A2A server/client.
- E2E: gated `kind` workflow.

Default `go test ./...` must not require Kubernetes, network access, real LLM calls, or long-running services.

## Operations

- Health endpoints on every service.
- Prometheus metrics for incidents, investigation duration, MCP calls, rollbacks, approval latency, and failures.
- OpenTelemetry traces across REST, A2A, MCP, and Kubernetes calls.
- Retry/circuit-breaker decorators around transient external ports.
