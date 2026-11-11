# Phase 7: Auth & Users (task breakdown)

Contract: `DEVELOPMENT_PLAN.md` "Phase 7", `.agents/07-auth-identity-agent.md`. Goal: real identity without changing the domain model. `Actor`, the incident state machine, the audit schema and the MCP/A2A surfaces do not change.

## Decisions (2026-11-10)

1. **Roles are DB-assigned.** `users.role` is `viewer`, `operator` or `administrator`. A user in the allowed GitHub organization is created as `viewer`. Accounts in `AUTH_BOOTSTRAP_ADMINS` (matched by numeric GitHub ID) become `administrator` on first login. Administrators change roles afterwards.
2. **One session, two carriers.** After the OAuth callback the server issues an opaque session token. It is accepted as an `HttpOnly` cookie (browser) or `Authorization: Bearer` (curl, scripts). Only the SHA-256 of the token is stored.
3. **E2E uses a fake GitHub.** A small stub (a standalone loopback process, see "End-to-end with a real session") serves the token, user and org endpoints behind a configurable GitHub base URL. The real callback, org check, session and role path run; only GitHub is faked.

## Design

- `Actor` is unchanged. A user is `Actor{id: "github:<numeric id>", human}`. The role is not in `Actor`; it is looked up per request, so a role change or revocation applies to the next request.
- Domain adds `Role` (ordered, `ParseRole`), `User`, and `Action` (create, read, reject, cancel, approve, admin). A pure `domain.RolePolicy` maps role and action to allow or deny. Approving a production rollback requires `administrator`; create, reject and cancel require `operator`; reads require `viewer`.
- The application layer owns a small `AccessPolicy` interface (`Authorize(ctx, actor, action) error`). The Postgres-backed implementation resolves the live role. `ApprovalService` calls it before the transaction. Denial is a typed domain error (REST 403).
- `SessionAuthMiddleware` is the second `httpserver.AuthMiddleware`. It resolves token to session to user on every request (join on `users`), rejects revoked, expired or disabled sessions with 401, and injects the typed `Actor`. `/health/*` and `/metrics` stay open. Handler signatures do not change.
- `AUTH_MODE=local|github`. `local` keeps today's fixed actor for dev. `github` fails closed at startup when client id, secret, redirect URL, allowed org or session TTL is missing.
- OAuth: authorization code with `state` and PKCE, `golang.org/x/oauth2`. Org membership is checked at login; a non-member gets no session. The GitHub access token is used once and never stored or logged.
- Scope: orchestrator REST only. Investigator and MCP servers remain network-trusted, as documented.
- Never logged or persisted: client secret, session tokens, GitHub tokens, auth headers.

## Sessions (7.3)

Runtime behaviour of `application.SessionService` and `httpserver.SessionAuthMiddleware`. `cmd/orchestrator` wires them in `AUTH_MODE=github` (see "GitHub login").

- **Token.** 32 bytes from `ports.RandomSource` (crypto/rand), unpadded base64url (43 characters). Only `SHA-256(token bytes)` is stored. `Issue` returns the raw token once; nothing stores, logs or formats it. A disabled user gets no session.
- **Carriers.** `Authorization: Bearer <token>` wins whenever an `Authorization` header is present; a bad header is not replaced by the cookie. Otherwise the `rollback_guard_session` cookie is used (two such cookies are rejected). The cookie is `HttpOnly`, `SameSite=Strict`, `Path=/`, expires with the session, and is `Secure` unless `SESSION_COOKIE_SECURE=false`. `ClearSessionCookie` clears it on logout. The middleware removes the `Authorization` header and the session cookie before calling any handler, public ones included.
- **Per request.** Token, session and user are resolved on every request in one lookup, so revocation, disable and role change apply to the next request. On success the `Actor` (`github:<id>`) is injected under the existing key (`ActorFromContext`) and the `Principal` (live user incl. role, session ID) under `PrincipalFromContext`. No last-seen write happens on the request path.
- **Failures.** Missing, malformed, unknown, expired or revoked token and disabled user all return the same `domain.UnauthenticatedError` and the same response: `401`, `WWW-Authenticate: Bearer`, `{"error":"unauthenticated","message":"authentication required"}` (plus `requestId`). A resolver infrastructure error (database down) returns `500 internal_error`, not 401, and only a fixed classification is logged.
- **CSRF.** A cookie-authenticated `POST`, `PUT`, `PATCH` or `DELETE` must carry an `Origin` (or, without `Origin`, a `Referer`) whose host equals the request `Host`, or neither header; otherwise `403 forbidden`, checked before the session lookup. Bearer requests are exempt. The comparison is against `Host` as received, so a reverse proxy must preserve `Host`.
- **Public paths.** Exact match only: `/health/live`, `/health/ready`, `/metrics`, `/v1/auth/github/login`, `/v1/auth/github/callback`. Everything else, including `/metrics/x` or `/health/live/`, needs a session.

| Variable | Default | Bounds |
|---|---|---|
| `SESSION_TTL` | `8h` | `5m` to `720h` (30 days) |
| `SESSION_COOKIE_SECURE` | `true` | `true` or `false` (plain-HTTP local development only) |

## GitHub login (7.4)

**Production must set `AUTH_MODE=github`.** The default `local` injects the fixed `LOCAL_ACTOR_ID` on every request and exists only for local development. In `local` mode the login routes are not registered (404) and the GitHub variables are ignored without validation. In `github` mode every variable below is validated at startup and the orchestrator refuses to start on any problem (fail closed). It logs the auth mode at startup (no secrets), and logs a warning when `SESSION_COOKIE_SECURE=false`.

- **Login** (`GET /v1/auth/github/login`). Draws a 256-bit `state` and a 256-bit PKCE verifier from crypto/rand and seals them with an expiry into the `rollback_guard_oauth_state` cookie. The seal is HMAC-SHA256 keyed by `AUTH_STATE_SECRET`. The cookie is `HttpOnly`, `SameSite=Lax` (the callback is a cross-site top-level redirect), `Path=/v1/auth/github`, lives 10 minutes, and is `Secure` per `SESSION_COOKIE_SECURE`. The response is `302` to GitHub with scopes `read:org user:email` and `code_challenge_method=S256`. There is no `return_to` parameter.
- **Callback** (`GET /v1/auth/github/callback`). Clears the state cookie on every outcome, then performs these steps in order:
  1. Verify the MAC and expiry, then compare `state` in constant time. On failure: `400`, and GitHub is not called.
  2. Exchange the code with the verifier. A rejected or expired code returns `400`.
  3. Read `/user` and `/user/orgs`, then discard the access token.
  4. Check membership in `AUTH_GITHUB_ALLOWED_ORG` (case-insensitive). A non-member gets `403`, and no user row or session is written.
  5. In one transaction, upsert the user and issue the session. A new user is `viewer`, or `administrator` when its numeric GitHub ID is in `AUTH_BOOTSTRAP_ADMINS`; that elevation applies only at creation. An existing user has login, email and organizations refreshed under an optimistic lock with bounded retry, and keeps the role. A disabled user gets `403`.
  6. Respond `200` with the session body and session cookie, `Cache-Control: no-store` and `Referrer-Policy: no-referrer`.
- **Failures.** Response bodies are fixed per class and never say which check failed. Only the class is logged: never the code, state, cookie, token, client secret or provider body. Any GitHub failure, including a timeout or an unexpected response, returns `502 bad_gateway`.
- **Bounds.** Each GitHub request is limited by a 10 s timeout (`config.GitHubHTTPTimeout`) and a 1 MiB response limit; redirects are not followed. Organizations are read 100 per page for at most 5 pages. A member whose allowed organization is not within the first 500 is denied (fail closed).
- **Resilience.** The token exchange is not retried, because an authorization code is single-use. A failed login is retried by the user starting again.
- **Leaving the organization.** Membership is checked only at login. A known user who is denied at login because they left the organization has all open sessions revoked; the user row is kept, so re-admission restores access. Until that user tries to log in again, existing sessions stay valid up to `SESSION_TTL`. An administrator revokes the user's sessions (7.5) to cut access at once.
- **Login collisions.** Users are keyed by GitHub ID. If a GitHub login was renamed and then taken by another account, the new account gets `409 conflict` until the old account signs in again (which refreshes its stored login).
- **Accepted risks.** The email is the public profile field and is display-only; it never feeds an authorization decision. Bootstrap admins are matched by numeric GitHub ID, never by login (a renamed login can be taken by another account); the login in each entry is only a readable label. The state cookie is not bound to a browser session (see "Known gaps").
- **Fake GitHub.** `internal/adapters/githuboauth/githubfake` serves the authorize, token, user and org endpoints for tests; 7.6 runs it standalone for the e2e stack.

| Variable | Default | Rule (github mode) |
|---|---|---|
| `AUTH_MODE` | `local` | `local` or `github` |
| `AUTH_GITHUB_CLIENT_ID` | none | required, printable ASCII, at most 128 characters |
| `AUTH_GITHUB_CLIENT_SECRET` | none | required; never logged or rendered |
| `AUTH_GITHUB_REDIRECT_URL` | none | required, absolute, HTTPS unless loopback, no credentials, query or fragment; must match the OAuth app |
| `AUTH_GITHUB_ALLOWED_ORG` | none | required, GitHub organization name |
| `AUTH_BOOTSTRAP_ADMINS` | none | required, comma-separated `login:githubID` entries (at most 32); both parts validated, only the numeric ID is matched |
| `AUTH_STATE_SECRET` | none | required, at least 32 bytes; never logged or rendered |
| `AUTH_GITHUB_BASE_URL` | `https://github.com` | HTTPS unless loopback |
| `AUTH_GITHUB_API_URL` | `https://api.github.com` | HTTPS unless loopback |

## Authorization, audit and user administration (7.5)

- **Enforcement.** Every human use case calls `AccessPolicy.Authorize(ctx, actor, action)` inside the application service before any state change; handlers only supply the session actor. `github` mode wires `RoleAccessPolicy`, which loads the user live on each call: unknown or disabled is `401`, a role below the action's minimum is `403 forbidden`, and a non-`github:<id>` actor is denied. `local` mode wires `LocalAccessPolicy`. Viewer reads; operator also creates, rejects and cancels; only an administrator approves a rollback (the recorded approver is the authorized actor) or administers users. A role change applies on the caller's next request. System transitions never consult the policy, and a system actor cannot use a human use case. Reads take the actor, so `QueryService` methods gained an actor parameter.
- **Routes** (`github` mode only; `404` in `local`).
  - `POST /v1/auth/logout`: revokes the caller's session, clears the cookie, `204`. Needs no role.
  - `GET /v1/me`: `{actorId, login, role}` with the live role.
  - `PATCH /v1/users/{userId}/role` (administrator): body `{"role": ...}`, strict JSON, at most 1 KiB. `userId` is the public actor ID `github:<id>`; a malformed ID is `400`, an unknown user `404`. Returns the target's identity. Setting the current role is a no-op and writes no audit row.
  - `POST /v1/users/{userId}/sessions:revoke` (administrator): revokes every open session of the user and returns `200 {"revoked": n}`.
  - `POST /v1/users/{userId}/disable` and `POST /v1/users/{userId}/enable` (administrator): return `200` with the target's summary `{actorId, login, role, disabled}`. Disable sets the user's disabled flag and revokes every open session of theirs in the same transaction, so the next request is `401` and a new login is refused (`403`) until enabled. Enable clears the flag only; revoked sessions stay revoked. Either call on a user already in that state is a no-op and writes no audit row. A malformed ID is `400`, an unknown user `404`.
- **Last administrator.** A role change or disable that would leave no enabled administrator, including self-demotion or self-disable, is `409 conflict`. The service locks the enabled administrator rows (`SELECT ... FOR UPDATE`) and re-checks inside the transaction that the caller is still an enabled administrator, so two concurrent demotions cannot both succeed.
- **Audit.** Migration `000006` adds `auth_audit_events`: `id, occurred_at, actor_id, action, target_user_id, from_role, to_role, session_count`. Actions are `role_changed`, `sessions_revoked` (written even when `n` is 0) and `logout`. A shape `CHECK` ties the nullable columns to the action, and a trigger rejects `UPDATE` and `DELETE` (append-only). Each row is written in the same transaction as the change it records, so a failed audit write rolls the change back. Rows hold no token, token hash, header or email. The incident `operation_events` schema is unchanged; those events now carry `github:<id>`. The down migration refuses to run while audit rows exist. Migration `000007` adds the actions `user_disabled` (with `session_count`, the sessions revoked by the disable) and `user_enabled` (no roles, no count) to the action and shape constraints; its down migration refuses to run while rows of those actions exist.
- **Contract.** `test/contract` checks that every OpenAPI operation is routed in `github` mode and that, behind the real session middleware, only the OAuth entry points, `/health/*` and `/metrics` answer without a session.

Open gaps of this area are consolidated in "Known gaps" at the end of this document.

## End-to-end with a real session (7.6)

`make e2e` and `make e2e-decline` run the Phase 6 flows through the real login path. Only GitHub is faked. Commands and variables: `docs/local-demo-env.md`, "Authentication mode".

- **Fake GitHub.** `test/tools/fake-github` serves `githubfake` (authorize with PKCE check, token, `/user`, `/user/orgs`) as a standalone loopback process. It reads `FAKE_GITHUB_ADDR` (non-loopback is refused), `FAKE_GITHUB_CLIENT_ID`, `FAKE_GITHUB_CLIENT_SECRET` and `FAKE_GITHUB_USERS` (`login:id:org+org;...`) from the environment, and prints only the user count and URL. No service image builds it. Its authorize endpoint honours GitHub's `login` parameter, which lets a script choose the account without a browser.
- **Users** (allowed organization `demo-org`): `demo-administrator` (1001, the only `AUTH_BOOTSTRAP_ADMINS` entry), `demo-operator` (1002), `demo-viewer` (1003), `demo-outsider` (1004, member of `other-org` only). New members are viewers, so after logging in `auth-login.sh` has the administrator promote the operator through `PATCH /v1/users/github:1002/role`: the role path is real too.
- **Login helper.** `scripts/demo/auth-login.sh` follows `GET /v1/auth/github/login` (302, state cookie in a private jar), requests the fake's authorize URL with `login=demo-<name>` (302 with the code) and completes `GET /v1/auth/github/callback` with the cookie. URLs carrying `state` or `code` go to `curl` on stdin; only HTTP statuses are printed. Tokens are kept in `.demo/auth-tokens` (`0600`, inside the `0700`, gitignored `.demo/`) and sent as `Authorization: Bearer` via a curl config on stdin.
- **Secrets.** The client secret and `AUTH_STATE_SECRET` are random per `services-up`, passed through process environments only. Nothing is committed, written or echoed. `scripts/demo/check-no-tokens.sh` (a CI step) fails if any stored session token appears in the service logs or captured e2e output.
- **Asserted** (`e2e.sh`, mirrored by `test/e2e/auth_test.go`): anonymous `401`; each role's `GET /v1/me`; non-member login `403` and no session; viewer create `403`; logout `204` then `401` on that token while another session of the same user stays valid; operator create; operator approve and viewer reject on an awaiting proposal `403` with the incident unchanged; administrator approve; audit actors `github:1002` (creation) and `github:1001` (approval, kind `human`). The decline scenario has no proposal, so only the creation actor and the absence of approval and execution events are asserted.
- **Default mode.** `AUTH_MODE` and `DEMO_AUTH` stay `local` for ordinary development (`make services-up`); the e2e targets force `github`.

## Tasks

Each task: one branch `phase7/7.N-name`, TDD, one commit per task, full gate at the end, one reviewer.

| Task | Content | Tier | Reviewer |
|---|---|---|---|
| 7.0 | ADR and OpenAPI draft: `/v1/auth/github/login`, `/callback`, `/v1/auth/logout`, `/v1/me`, admin `PATCH /v1/users/{id}/role`, `POST /v1/users/{id}/sessions:revoke`, security schemes, 401/403. Contract test updated. | standard | none |
| 7.1 | `domain.Role`, `User`, `Action`, `RolePolicy`, role-denied error. Table-driven tests. | heavy | none (7.8 covers) |
| 7.2 | Migration `000005` (`users`, `sessions`), Postgres repositories, expiry and revocation queries, integration tests. | heavy | sql-reviewer |
| 7.3 | Session service (issue, resolve, revoke, logout), `SessionAuthMiddleware`, hashing, TTL constants. | heavy | security-policy-reviewer |
| 7.4 | GitHub OAuth adapter, login and callback handlers, org allow-list, bootstrap admins, config validation. | heavy | security-policy-reviewer |
| 7.5 | `AccessPolicy` in the application services, audit events carry the real actor, `/v1/me` and admin endpoints. | heavy | security-policy-reviewer |
| 7.6 | Fake GitHub stub, e2e and e2e-decline driven by a real session, CI job, docs (`docs/phase7-auth.md` runtime section, `local-demo-env.md`). | standard | qa-test-reviewer |
| 7.7 | Phase exit: `invariant-review`, full gate, exit-gate evidence. | standard | none |

7.1 to 7.5 are strictly sequential (each builds on the last). 7.0 can run first. Expected cost from earlier heavy runs: about $2-4 per heavy task.

## Exit gate

- An authorized GitHub org member can create an incident; a viewer cannot.
- Only an administrator can approve a production rollback; an operator gets 403.
- Audit events show the real actor ID.
- Revoking a session, disabling a user or changing a role takes effect on the next request.
- `make e2e` and `make e2e-decline` pass driven by a real session against the fake GitHub.
- No secret or token appears in logs, events, A2A messages or MCP results (test-pinned).

## Exit gate evidence (7.7)

Run on branch `phase7/7.7-exit` (2026-11-11, agent sandbox, Go 1.26.10, fresh `kind` cluster `rollback-guard` created for this run). Every Go gate used `-count=1`.

| Gate | Result |
|---|---|
| `make lint` | 0 issues |
| `go test -race -count=1 ./...` | 31 packages ok, none failed |
| `go test -count=1 ./test/contract/...` | ok |
| phase3, phase4, phase5 integration (`-tags=integration -count=1`) | ok; 0 `SKIP` lines in `-v` output |
| `make postgres-integration` | `test-no-skips: 93 passed, 0 skipped` |
| `govulncheck ./...` | **FAILS**: 1 standard-library finding (GO-2026-6859, `net/http`), fixed in go1.26.11; the sandbox toolchain is go1.26.10. No finding in a dependency. Not caused by Phase 7 code; needs the toolchain bump. |
| `make e2e` (fresh kind cluster) | PASSED in 50s (incident `incident-1-a57c558e`, resolved) |
| `make e2e-decline` | PASSED in 6s (incident `incident-22-4b017111`) |
| Token scan of real e2e output | `scripts/demo/check-no-tokens.sh` was not run in the exit session (the harness required approval for it). An equivalent check was run instead: `grep -rlF` with the 4 tokens from `.demo/auth-tokens` over both captured e2e logs and `.demo/logs` found no match. CI runs the script itself. |

`make e2e-test` (the Go `-tags=e2e` suite) was not run in this session; the shell e2e asserts the same checks.

Invariant review over `git diff 10deef8..HEAD`: no FAIL. Notes: `AUTH_MODE` defaults to `local`, and `Authorize` and `recordDecision` use separate transactions (both under "Known gaps").

| Exit-gate bullet | Proof | Status |
|---|---|---|
| An authorized org member can create an incident; a viewer cannot | `TestHTTPRoleMatrix`, `TestRoleAccessPolicyMatrix`, `TestRolePolicyMatrix`, `TestHumanUseCasesAuthorizeBeforeAnyTransaction`, `TestLoginMemberIsCreatedAsViewerWithWorkingSession`, `TestLoginNonMemberIsDeniedWithoutRows`, `TestGitHubLoginRoleEnforcementAndAuditOnPostgres`; `make e2e` step "authentication and roles" (viewer create 403, operator create ok, outsider 403 with no session) | Proven |
| Only an administrator approves a production rollback; an operator gets 403 | `TestRoleEnforcementOnApprovalPath`, `TestHTTPRoleMatrix`; `make e2e` step "approval is administrator-only" (operator and viewer 403, incident unchanged, administrator approves) | Proven at the REST and application layer. **Gap:** the approver's role is not re-checked at MCP execution. |
| Audit events show the real actor ID | `TestHTTPIncidentEventsRecordTheSessionActor`, `TestApproveAndExecuteAuditEventsCommitWithTheirStateChange`, `TestAuthAuditAppendListAndShape`; `make e2e` step "audit names the real GitHub actors" (`github:1002` creation, `github:1001` approval) | Proven |
| Revoking a session, disabling a user or changing a role takes effect on the next request | `TestHTTPSessionRevocationAndDisableTakeEffectOnNextRequest`, `TestSessionAuthMiddlewareRejectsDisabledUserImmediately`, `TestSessionResolveSeesLiveRole`, `TestRoleAccessPolicyReadsTheLiveRole`, `TestSessionAuthMiddlewareOnPostgres`, `TestHTTPDisableEndpointTakesEffectOnNextRequest` (`POST .../disable` through the real router, session middleware and `AdminService`, then `401` on the disabled user's next request and their sessions stay revoked after enable), `TestAdminDisableEnableUserOnPostgres`; e2e logout then 401 on the same token | Proven |
| `make e2e` and `make e2e-decline` pass with a real session against the fake GitHub | Both run above on a fresh kind cluster with `DEMO_AUTH=github` | Proven locally. The CI `e2e` job for this exact commit was not observed. |
| No secret or token appears in logs, events, A2A messages or MCP results | Logs: `TestSessionAuthMiddlewareNeverLogsCredentials`, `TestSecretsNeverRender`, `TestErrorMappingIsStableAndDoesNotLeakDetails`, the token scan above. Stored data: `TestAuthMigrationStoresOnlyTokenHashes`, `TestAuthAuditMigrationHasNoSecretColumns`. Credentials never reach handlers: `TestSessionAuthMiddlewareHidesCredentialsFromHandlers`. | **Partly proven.** No test scans A2A messages, MCP results or `operation_events` for a session token. They are protected structurally (the middleware strips credentials, `Actor` carries only `github:<id>`), but that is not test-pinned end to end. |

## Known gaps

Open or accepted at Phase 7 exit.

1. **Login rate limiting.** `/v1/auth/github/login` and `/callback` are not rate-limited. Use a proxy rate limit in any exposed deployment.
2. **Approver role at MCP execution.** The rollback-mode Kubernetes MCP re-checks the pending execution, proposal, policy decision and approval record, not the approver's current role. An approval made by an administrator stays executable after a demotion, bounded by `APPROVAL_TTL`.
3. **Two-transaction approve race.** `Approve` authorizes against the auth store, then records the approval in the incident store. An administrator demoted between the two can still record one approval. Closing it needs gap 2's re-check.
4. **`OPENAI_CUSTOM_HEADERS`.** The OpenAI SDK reads it from the environment, so custom headers other than `Authorization` still reach the configured endpoint. Needs a header allow-list in the transport (`docs/refactoring-plan.md`).
5. **Token check opens a read-write transaction.** Session resolution runs through the unit of work on every request although it only reads.
6. **CSRF host comparison behind proxies.** The `Origin` or `Referer` host is compared with `Host` as received; a reverse proxy must preserve `Host` or cookie-authenticated writes get 403. Bearer requests are exempt.
7. **State cookie not bound to a browser session.** An attacker who can plant cookies for the host (for example from a sibling subdomain) could perform a login CSRF into their own account. Serve the orchestrator from a host without untrusted sibling subdomains.
8. **`AUTH_MODE=local` default.** It injects one fixed human actor that can approve. Nothing refuses it on a non-loopback bind; the server only logs a warning, and that warning says "has no authentication" even in `github` mode. Production must set `AUTH_MODE=github`.
9. **Open `/metrics`.** It is a public path so Prometheus can scrape it without a session.
10. **Audit reads.** No API lists auth audit rows; read `auth_audit_events` directly.
11. **Token absence in A2A messages, MCP results and `operation_events` is not test-pinned** (last exit-gate row).
12. **Leaving the organization** is noticed only at the user's next login; an administrator disables the user (or revokes their sessions) to cut access earlier.
13. **`govulncheck`** fails on the sandbox toolchain (GO-2026-6859, fixed in go1.26.11); not a Phase 7 change.
