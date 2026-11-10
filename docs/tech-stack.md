# Rollback Guardian Tech Stack

## Go

Use modern Go with standard library defaults where practical:

- `net/http` and Go 1.22+ method/path mux for HTTP.
- `log/slog` for structured logs.
- Manual constructor injection.
- Table-driven tests.

## Protocols

- A2A: official `github.com/a2aproject/a2a-go/v2` SDK, HTTP+JSON REST binding.
- MCP: official `github.com/modelcontextprotocol/go-sdk`, Streamable HTTP transport.
- JSON schema: prefer typed Go structs and generated/derived schemas over unstructured `map[string]any`.

## Kubernetes and Metrics

- Kubernetes API: `k8s.io/client-go`.
- Local cluster: `kind`, manifests applied with kustomize.
- Metrics: Prometheus, queried by the Observability MCP through `prometheus/client_golang`. `demo-api` exposes the request counter and latency histogram those queries select.
- Images: multi-stage builds onto distroless, non-root.

## Persistence

- PostgreSQL is canonical for v1.
- Use `pgx`/`pgxpool`.
- Use migrations.
- `sqlc.yaml` is present, but nothing is generated; repositories use `pgx` directly. Generate only if query volume grows enough to justify it.
- Migrations are applied with `golang-migrate`.
- Use Testcontainers for integration tests.

SQLite appeared in earlier source notes as a zero-friction alternative, but it conflicts with the technical specification and development plan. Do not choose SQLite unless a future ADR explicitly supersedes the PostgreSQL decision.

## LLM Providers

Keep model access behind one port. Reasonable implementations:

- Rule-based diagnosis: the default and the fallback for every other strategy.
- One implemented transport: the official OpenAI Go SDK against any OpenAI-compatible `/v1/chat/completions` API (`LLM_PROVIDER=openai_compatible`). That covers hosted APIs such as Groq or OpenRouter and a local Ollama.
- Provider-specific adapters (for example Gemini) are not implemented.

The LLM may only produce structured diagnosis data. It may not call tools, approve, execute, or bypass validation.

## Testing

- Unit: pure domain and application logic.
- Contract: REST/OpenAPI, A2A Agent Card/tasks, MCP tool schemas.
- Integration: PostgreSQL, MCP server/client, A2A server/client.
- E2E: gated `kind` workflow. The live-cluster adapter check exists (`make phase6-kind-check`, build tag `kind`); the full unattended flow is still to be built.

Default `go test ./...` must not require Kubernetes, network access, real LLM calls, or long-running services.

## Operations

- Health endpoints on every service.
- Prometheus metrics (`prometheus/client_golang`, per-process registry) for HTTP, incidents, investigations, A2A and MCP calls, diagnoses, rollbacks, and verifications.
- OpenTelemetry traces across REST, A2A, MCP, and Kubernetes/Prometheus calls; OTLP export when configured.

## Resilience

Retry and circuit-breaker decorators around transient external ports live in `internal/platform/resilience` (design and settings: `docs/phase6-resilience.md`). They are written in-repo (about 600 lines, no third-party dependencies) instead of using `sony/gobreaker` and `sethvargo/go-retry`. Reasons:

- The rules that matter are project-specific and must be unit-tested: what is transient, what a mutation may retry, retry never sleeping past a context deadline, a caller's cancellation being neutral for a breaker, and an open LLM breaker reading as `ErrReasonerUnavailable`. A library would still need a wrapper for each, and `gobreaker` reads the wall clock internally, so half-open timing is hard to test without sleeping.
- No new dependency means no new `govulncheck` surface or supply-chain review.
- The decorators are generic functions over our ports, so no port signature changes.

Revisit if breaker needs grow beyond consecutive-failure counting (for example rate-based tripping), where `gobreaker` would then be the better choice.

Planned for the rest of Phase 6:

- None for resilience; the unattended E2E flow and CI remain.
