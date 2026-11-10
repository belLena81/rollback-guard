# Phase 7: Auth & Users (task breakdown)

Contract: `DEVELOPMENT_PLAN.md` "Phase 7", `.agents/07-auth-identity-agent.md`. Goal: real identity without changing the domain model. `Actor`, the incident state machine, the audit schema and the MCP/A2A surfaces do not change.

## Decisions (2026-11-10)

1. **Roles are DB-assigned.** `users.role` is `viewer`, `operator` or `administrator`. A user in the allowed GitHub organization is created as `viewer`. Logins in `AUTH_BOOTSTRAP_ADMINS` become `administrator` on first login. Administrators change roles afterwards.
2. **One session, two carriers.** After the OAuth callback the server issues an opaque session token. It is accepted as an `HttpOnly` cookie (browser) or `Authorization: Bearer` (curl, scripts). Only the SHA-256 of the token is stored.
3. **E2E uses a fake GitHub.** A small in-test stub serves the token, user and org endpoints behind a configurable GitHub base URL. The real callback, org check, session and role path run; only GitHub is faked.

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

Runtime behaviour of `application.SessionService` and `httpserver.SessionAuthMiddleware`. `cmd/orchestrator` wiring (`AUTH_MODE`) arrives in 7.4.

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
