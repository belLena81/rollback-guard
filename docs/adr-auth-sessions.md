# ADR: Roles and server-side sessions for the orchestrator REST API

Status: accepted (2026-11-10). Task breakdown: `docs/phase7-auth.md`.

## Context

Until Phase 7 a middleware injects a fixed local human `Actor`. Phase 7 adds real identity
without changing `Actor`, the incident state machine, the audit schema, or the MCP/A2A
surfaces. Removing a user's access must take effect on the very next request, which rules
out stateless tokens. Scope is the orchestrator REST API only; the investigator and MCP
servers stay network-trusted.

## Decisions

1. **Roles are DB-assigned.** `users.role` is `viewer`, `operator` or `administrator`. A
   member of the allowed GitHub organization is created as `viewer`. Logins listed in
   `AUTH_BOOTSTRAP_ADMINS` become `administrator` on first login. Administrators change roles
   afterwards (`PATCH /v1/users/{userId}/role`).
2. **One opaque session token, two carriers.** After the OAuth callback (authorization code,
   `state`, PKCE) the server issues a random token, accepted as an `HttpOnly` cookie or as
   `Authorization: Bearer`. Only its SHA-256 is stored. The GitHub access token is used once
   for the org check and is never stored or logged.
3. **E2E uses a fake GitHub.** An in-test stub serves the token, user and org endpoints
   behind a configurable base URL. The real callback, org check, session and role path run.

## Role/action matrix

| Action | viewer | operator | administrator |
|---|---|---|---|
| read incidents and events, `GET /v1/me` | yes | yes | yes |
| create, reject, cancel incident | no | yes | yes |
| approve a production rollback | no | no | yes |
| change roles, revoke sessions | no | no | yes |

Denial returns `403 forbidden`; a missing, expired, revoked or disabled session returns
`401 unauthenticated`. Both use the existing error envelope.

## Per-request live role lookup

The role is not stored in `Actor` or the session. Each request resolves token, session and
user in one join and evaluates `domain.RolePolicy` against the current role. A role change,
user disable or session revocation therefore applies to the next request, with no cache to
invalidate.

## Consequences

- One extra indexed query per request; accepted for instant revocation.
- No domain, handler-signature or audit-schema changes; auth is a second `AuthMiddleware`
  plus an `AccessPolicy` check before the approval transaction.
- Coarse three-role model only; no per-resource permissions.
- `AUTH_MODE=local` keeps the fixed dev actor; `github` fails closed on missing config.
- Session tokens, OAuth secrets and GitHub tokens are never logged or persisted in clear.
- The token appears once in the callback JSON body for scripts; clients must treat it as a secret.
