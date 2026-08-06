# Phase 5 AI Diagnosis

Phase 5 adds optional LLM and hybrid diagnosis to the Investigator. The Orchestrator, policy, approval, rollback mutation, and recovery verification paths remain deterministic and provider-independent.

## Strategy Boundary

`DIAGNOSIS_STRATEGY` selects one implementation of the existing `ports.Diagnoser` interface:

- `rule_based`: deterministic rules only and no provider configuration.
- `llm_based`: one model reasoning request followed by application validation; invalid or unavailable output falls back to rules.
- `hybrid`: rules produce the initial candidate, the model reviews it, and invalid or unavailable output returns that exact rule candidate.

The provider-neutral `ports.Reasoner` contract contains normalized incident facts, evidence, an optional rule candidate, and untrusted diagnosis output. OpenAI SDK and HTTP types remain in `internal/adapters/llm`.

The model receives no tools or infrastructure credentials. It cannot approve, execute, or verify a rollback. Its output must still pass domain construction, application evidence-reference validation, deterministic policy, exact proposal-hash approval, scoped MCP mutation, and recovery verification.

## Provider Request

The initial transport uses the official OpenAI Go SDK against an OpenAI-compatible `/v1/chat/completions` API. It sends one request with retries disabled and strict JSON Schema output containing only:

- `hypothesis`
- `confidence`
- `evidence_ids`
- `recommendation` (`rollback` or `no_action`)

Only allow-listed normalized fields are sent. Raw logs and free-form Kubernetes event messages are excluded. Prompts, authorization headers, API keys, raw provider bodies, and provider error text are never persisted, returned, or logged.

## Runtime Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `DIAGNOSIS_STRATEGY` | `rule_based` | `rule_based`, `llm_based`, or `hybrid` |
| `LLM_PROVIDER` | `openai_compatible` | Provider transport selected at composition |
| `LLM_BASE_URL` | none | Required API base URL for model strategies, including `/v1` |
| `LLM_MODEL` | none | Required model identifier for model strategies |
| `LLM_API_KEY` | none | Required for non-loopback providers; optional for local compatible APIs |
| `LLM_TIMEOUT` | `15s` | One model attempt; must be at most `30s` and below `A2A_TASK_TIMEOUT` |
| `LLM_MAX_OUTPUT_TOKENS` | `512` | Output limit in the range `64..2048` |

Remote providers require HTTPS. HTTP is accepted only for `localhost` or another loopback address. Provider health is deliberately excluded from Investigator readiness because runtime failure uses the deterministic rule fallback.

## Provenance And Compatibility

Every conclusion records effective strategy plus sanitized provenance: requested strategy, model, prompt version, input/output token counts, and an optional fallback category. Fallback conclusions use effective strategy `rules` and retain the requested `llm` or `hybrid` strategy.

The A2A investigation extension remains schema v1. The provenance field is optional only so new readers can recover cached Phase 4 rule results. Existing strict Phase 4 readers reject new model-backed results, so deploy the updated Orchestrator before enabling or deploying an Investigator configured for `llm_based` or `hybrid`.

Apply migration `000004_ai_diagnosis.up.sql` before enabling Phase 5. Its down migration refuses to run while model-backed results exist.

## Verification

Default tests use provider-neutral fakes and an injected HTTP transport; they never contact a real provider.

```bash
GOCACHE=/tmp/rollback-guard-go-cache go test ./...
GOCACHE=/tmp/rollback-guard-go-cache go test -race ./...
GOCACHE=/tmp/rollback-guard-go-cache make lint
GOCACHE=/tmp/rollback-guard-go-cache make phase5-integration
```
