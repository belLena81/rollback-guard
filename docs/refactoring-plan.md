# Refactoring Plan

Source: a code-quality audit of the `phase6` branch (production code under `cmd/` and `internal/`), combining an extended `golangci-lint` run with two read-only reviews (library usage; duplication, hardcoded values, and structure). Findings marked **verified** were re-read in the code after the review.

## Rules for every stage

- One stage per branch, in the order below. Behaviour-preserving unless the stage says it fixes a defect.
- Defect fixes start with a failing test. Pure refactorings start green and stay green.
- Each stage ends with the full gate (`verify-gates` skill): lint, race tests, contract tests, the Phase 3-5 integration suites, guard tests with zero skips, `govulncheck`.
- No stage changes an MCP tool catalog, the A2A schema, the REST contract, or a migration that is already committed. If one must, it is called out below.
- When a stage is done, its linter is switched on in `.golangci.yml` so the problem cannot return (see "Lint ratchet").

## Sequencing with Phase 6

| Order | Work | Why here |
|---|---|---|
| 1 | Phase 6 telemetry (in progress) | Touches the same `main.go` files and adapters; merge it first. |
| 2 | R1 Correctness defects (done) | Real bugs in persistence; cheap; must not ship into the E2E run. |
| 3 | R2 Process bootstrap and config (done) | Removes four-way duplication before the resilience decorators add more wiring. |
| 4 | Phase 6 resilience decorators | |
| 5 | R3 Approve-and-execute use case | The E2E flow drives exactly this path; fix its shape first. |
| 6 | Phase 6 E2E flow and CI | CI enforces the ratcheted lint config from here on. |
| 7 | R4-R8 | After the Phase 6 exit gate, before Phase 7 starts changing auth and policy. |

## R1. Correctness defects (done)

Merged. Eight of nine items were confirmed and fixed test-first. Item 8 was not a defect: the domain constructors already reject an unknown recommendation, and a test now pins that. `exhaustive`, `nilerr`, and `bodyclose` are enabled, and `golang.org/x/net` is at v0.62.0. Findings the R1 reviews raised outside its scope are carried into R2 and R3 below.

| # | Location | Defect | Fix |
|---|---|---|---|
| 1 | `internal/adapters/postgres/repositories.go:598` `eventRepo.Append` | **Verified.** After a failed insert it queries `MAX(sequence)` in the same transaction. PostgreSQL has already aborted the transaction, so the query always fails and `VersionConflictError` is never returned. | Insert with `ON CONFLICT DO NOTHING` and treat zero rows affected as the sequence conflict; or wrap the insert in a savepoint. Integration test with two writers on one incident. |
| 2 | `repositories.go:554` `idempotencyRepo.Create` | **Verified.** Every error, including cancellation and connection loss, becomes `IdempotencyConflictError`. | Return the conflict only for unique violation `23505` (as `mapError` already does); wrap everything else. |
| 3 | `internal/adapters/postgres/store.go:26` `WithinTransaction` | **Verified.** Manual begin/rollback/commit; a panic in the callback skips rollback and leaks the connection. | `pgx.BeginFunc`. Test that a panicking callback leaves the pool usable. |
| 4 | `internal/adapters/mcp/client.go:76` `RequireTools` | **Verified.** The fail-closed catalog check compares `fmt.Sprint` of two slices, which is ambiguous for names containing spaces. | `slices.Sorted` + `slices.Equal`. Table test including a name with a space. |
| 5 | `internal/adapters/kubernetes/client.go:428` | **Verified.** Lock release is reached through a type assertion on an unexported interface; any other `RollbackMutationGuard` silently skips release. | Add `Release` to `ports.RollbackMutationGuard`; update fakes. |
| 6 | `internal/adapters/httpapi/errors.go:69` `writeJSON` | **Verified.** Calls `http.Error` after `WriteHeader`; it has no effect. | Encode to a buffer first, or log the encode failure. |
| 7 | `internal/application/execution_service.go:138,140` | **Verified.** Raw `err.Error()` is persisted as the audit reason, while other paths use fixed reason constants. Risks leaking internals into the audit trail. | Map to fixed reason codes. |
| 8 | `internal/adapters/a2a/wire.go:221` | An unknown recommendation string silently becomes the zero value. | Reject unknown values. |
| 9 | Six non-exhaustive enum switches (`mcp/client.go:139`, `a2a/client.go:234`, `application/ai_diagnosis.go:168`, `cmd/investigator/main.go:154`, `domain/incident.go:31`) | New enum values fall through silently. | Add the missing cases or an explicit failing default; enable `exhaustive`. |

## R2. Process bootstrap and configuration (done)

Merged. Shared `httpserver.Serve` and health handler, `internal/platform/bootstrap`, config sub-loaders with named defaults and bounds, loopback default binding, `A2A_CONNECT_TIMEOUT`, tagged-file linting, and `postgres-integration` / `integration` make targets. Left for later: `DB_MAX_CONNS` int32 bound (R8.5), a 10s shutdown that is shorter than a 2m rollout watch, and widening `mnd` beyond `cmd/` and `internal/platform/` (seven literals remain).

Found while doing R2 (pre-existing, fixed separately before R3): the Investigator required an MCP catalog of exactly one tool per server, so it could never connect to the real servers. No end-to-end run had exercised that connection.

Findings: the HTTP server lifecycle is written four times (`cmd/*/main.go`), each setting only `ReadHeaderTimeout`; `connectMCP` twice; the pgx pool block three times; `run()` in the orchestrator is 163 lines with a 452-character statement. `config.go` parses `METRICS_WINDOW`, `DB_MAX_CONNS`, `DB_PING_TIMEOUT`, and `ALLOWED_TARGETS` three times each and discards the parse error in `err != nil || x > max` checks. 31 of the 50 magic numbers are inline defaults in `config.go`.

Changes:

1. `internal/platform/httpserver`: one `Serve(ctx, cfg, handler, logger)` with read, write, idle, and shutdown timeouts as named constants (model: `internal/demoapi/run.go`).
2. One shared health handler; the three current implementations (`httpapi/server.go:59`, `mcp/http.go:24`, `a2a/server.go:60`) return three different not-ready bodies and hardcode a 2s timeout in four places. Use the configured `DBPingTimeout`, which the investigator parses and never uses. Update `api/openapi.yaml` and its contract test if the body changes.
3. `internal/platform` helpers `NewPool` and `ConnectMCP`; split each `run()` into `buildMCP`, `buildServices`, `buildHTTP`.
4. `config.go`: sub-loaders (`loadDB`, `loadMCPClient`, `loadTargets`) and `boundedDurationEnv(name, def, min, max)` that reports the real parse error. All defaults and bounds become named constants declared once; the 30s..1h window and the 120s watch cap are currently repeated in five packages each.
5. Defaults that disagree:
   - Orchestrator and MCP servers bind all interfaces while `docs/phase4-a2a.md` says loopback. Decide one (loopback until Phase 7 is the safer choice) and make code and docs agree.
   - A2A connect reuses `MCP_CONNECT_TIMEOUT`; give it its own setting or rename the shared one.
   - `staleAfter` (2m) and the 100ms poll in `a2a/idempotent_investigator.go:28` are fixed; derive them from `A2A_TASK_TIMEOUT`.
6. Gate coverage found during R1:
   - `make lint` does not pass `--build-tags`, so integration- and kind-tagged tests are never linted (11 `errcheck` findings today). Lint with `integration,kind` tags and fix them.
   - No make target runs the Postgres adapter integration tests. Add `postgres-integration` (failing on any skip) and a single `integration` target that runs every tagged suite.

Item 5 continued:

   - The investigator identifies to MCP servers as `rollback-guardian-orchestrator` (`mcp/client.go:56`, **verified**); pass the client name in. Declare the version string once (it appears three times).

## R3. Approve-and-execute use case

Finding (**verified**): `internal/adapters/httpapi/server.go:169-240` orchestrates approve, execute, verify, and failure compensation, and mints the idempotency key `"approval:"+id`. That is application logic in a handler, against the "thin handlers" rule.

Also in this path, found by the R1 reviews:

- **Lock release ownership.** `Release` is keyed by idempotency key only, so a late deferred release from one request can unlock the lock held by a replay of the same key. The `resourceVersion` precondition and the ledger state check bound the impact, but ownership should be explicit: release only the attempt that acquired the lock.
- **Raw error text in 409 responses.** `writeError` returns `err.Error()` for conflict-class errors, so a failed cleanup can put database error text in a response body. Return fixed messages.
- **Encode-failure 500.** `writeJSON` has no request ID or logger, so the failure is neither correlated nor logged.

Change: an application use case `ApproveAndExecute` that owns the sequence, the compensation, and the key derivation. The handler decodes, calls, encodes. Existing HTTP tests keep passing unchanged; the flow gets application-level tests for each failure branch.

## R4. Application layer structure

1. **Dispatch state transitions.** `InvestigationDispatch` is mutated by hand: 6 `Version++`, 5 lease clears, 8 copies of the cancel check across the coordinator and services. Add methods (`Complete`, `Fail`, `Cancel`, `RequestCancel`, `CancelPending`) that own version, lease, and timestamps. Move the type and its enums out of `internal/ports/ports.go` (they carry behaviour) into `internal/domain`.
2. **Incident transition + audit event.** 12 `Incidents().Update` sites and 24 calls to a 7-parameter `appendEvent`, with `From` hardcoded in some and captured in others. One `transition(ctx, repos, id, actor, fn)` helper that derives From/To and appends the event.
3. **`workflow` struct.** Eleven dependencies shared by six empty service wrappers. Give each service its own dependency struct plus the shared transition helper, and one file per service (today `ApprovalService` lives in `investigation_service.go`, `Verify` and `Cancel` in `execution_service.go`, `appendEvent` in `query_service.go`).
4. **Complexity.** Split the functions over the threshold: `Gateway.Watch` (cognitive 65), coordinator `ProcessNext` (53, with a duplicated retry block), `recordDecision` (51), `complete` (47), `Execute` (33). Then enable `gocognit` at 30.
5. **Dead code.** `InvestigationService.Investigate`, `IncidentService.CreateIncident`, `QueryService.GetProposal`, `InvestigationGateway.Get` with `InvestigationTaskSnapshot`, unread `InvestigatorConfig.LLMProvider`, unused `cause` parameter in `newFailedExecution`. The orchestrator builds a rule diagnoser only to satisfy `NewServices`. Delete, or move to test helpers where tests need them.
6. **Clock and IDs.** Seven direct `time.Now()` calls in adapters bypass `ports.Clock`; the execution ID is derived twice with different prefixes (`execution-`, `mcp-mutation-`). Inject the clock; one ID function.

## R5. Domain vocabulary

1. **Enum parsing.** 15 parse functions in `postgres/codecs.go`, `investigation_tasks.go`, `rollback_mutation_guard.go`, re-implemented in `a2a/wire.go`, `a2a/client.go`, and `config.go`. Add a pure `ParseX` beside each domain `String()` and delete the copies. Adapters keep their own wire spellings only where they differ on purpose. Note `DIAGNOSIS_STRATEGY=rule_based` is stored as `rules`; document or unify.
2. **Symptom kind.** 15 bare string literals across the Prometheus adapter, MCP metrics adapter, and observability server; `domain.NewSymptom` accepts any non-empty kind, so an unsupported one fails late. Introduce a validated `domain.SymptomKind`.
3. **Metric and annotation names.** Share the Prometheus series names between `internal/demoapi` and `internal/adapters/prometheus` (the contract test already ties them together); name the repeated `"true"`, `"pending"`, `"unknown"` literals.

## R6. Adapter boilerplate

1. **MCP tool handlers.** The three-line `protocol.Failure[T]` return appears 32 times. One generic `handle[In, Out]` wrapper. Tool names and schemas must not change; `test/contract/mcp_tools_test.go` is the guard.
2. **pgx.** `pgx.CollectRows` for the four manual scan loops; batch or `unnest` the per-row evidence-reference inserts in `conclusionRepo.Save`.
3. **`/metrics`.** Replace the hand-written exposition string with `promhttp.HandlerFor`. Expected to be absorbed by the telemetry task; verify at merge.
4. **LLM adapter.** Replace the custom header-stripping `RoundTripper` with the SDK's `option.WithHeaderDel`, keeping the tests that prove no credentials leak.
5. **Persistence placement.** SQL lives in `adapters/a2a` (`idempotent_investigator.go`, `postgres_task_store.go`). Move it under `adapters/postgres` behind a small port, or record the exception in an ADR.

## R7. Standard-library modernisation

Mechanical, no behaviour change: `slices.Clone` (10 places), `slices.SortFunc` (3), `slices.Contains` (6), `new(false)` for the two `boolPointer` helpers, `model.Duration` for `durationLiteral`, drop the dead `crypto/rand` error branch, plus the `modernize`, `intrange`, `perfsprint`, and `unconvert` hints. Run the linters' auto-fix, review the diff, enable the linters.

## R8. Readability and error context

1. **Line length.** 75 production lines exceed 200 characters; several exceed 350. Break struct literals and call chains one field per line. Enable `lll` at 160.
2. **Error wrapping.** 295 returns pass through an error from another package with no context (113 in `internal/application`). Wrap at package boundaries with `%w` and an operation name; do not wrap domain errors that callers match with `errors.As` in a way that changes their identity. Enable `wrapcheck` with the domain and ports packages ignored.
3. **Repository context.** 27 of 28 repository methods take no `context.Context`; the Postgres adapter carries the transaction's context in the struct, and `NextSequence` takes one and ignores it. Cancellation works, so this is an inconsistency. Decide once: remove the stray parameter and document the transaction-scoped context in `ports.go`, or thread `ctx` through every method. Record the decision in an ADR.
4. **Detached contexts.** Seven `contextcheck` hits. The lock-release and cleanup paths detach on purpose; give each a comment and `context.WithoutCancel` plus a timeout. `a2a/server.go:64` logs with `context.Background()` inside a handler; use the request context.
5. **Integer narrowing.** Four `int -> int32` conversions from config (`gosec` G115); bound the value in config validation.
6. **`sqlc.yaml`.** Configured, nothing generated. Remove it, or generate. Do not leave it as dead configuration.

## Lint ratchet

Add to `.golangci.yml` as each stage lands, so CI holds the line:

| After | Enable |
|---|---|
| R1 | `exhaustive`, `nilerr`, `bodyclose` |
| R2 | `mnd` (config package first), `usestdlibvars` |
| R4 | `gocognit` (30), `gocyclo` (20), `nestif`, `unparam`, `dupl` |
| R7 | `modernize`, `intrange`, `perfsprint`, `unconvert`, `copyloopvar` |
| R8 | `lll` (160), `wrapcheck`, `contextcheck`, `gosec` |

## Checked and sound

No action needed: no global mutable state, no bare actor strings, no infrastructure imports in `internal/domain`, strict JSON decoding and body limits, `errors.Is/As` throughout with no sentinel `==`, `rows.Err()` checked everywhere, correct use of client-go selectors and API error helpers, typed MCP tool registration, and the deliberate strategic-merge rollback patch with a `resourceVersion` precondition. The small-interface rule holds (no port has more than four methods); the per-adapter target DTOs are intentional wire isolation.
