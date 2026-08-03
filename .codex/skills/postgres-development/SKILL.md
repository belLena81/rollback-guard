---
name: postgres-development
description: Design, review, or modify PostgreSQL migrations, schemas, indexes, constraints, queries, pgx/sqlc adapters, and data-access tests in Rollback Guardian using modern SQL and PostgreSQL best practices.
---

# PostgreSQL Development

Use for PostgreSQL schema, migration, query, index, constraint, transaction, and repository-adapter work.

## Repository Rules

- Keep PostgreSQL code in adapters and migrations: `internal/domain` and `internal/application` must not import SQL, `pgx`, database packages, or migration tooling.
- Implement existing ports directly; do not change domain/application interfaces to make SQL easier unless the current port is genuinely insufficient.
- Store only data required for the app workflow, invariants, or query paths. Do not add fields because they are generally useful for incident-management products.
- Prefer normalized relational columns for values used in identity, joins, filtering, uniqueness, or ordering.
- Use `jsonb` only for typed payloads that are not queried relationally, such as event details, policy rule snapshots, and evidence payload variants.
- Keep SQL fixed and code-owned. Never build dynamic SQL from request input unless all dynamic pieces are allow-listed by code.

## DDL Standards

- Use PostgreSQL enum types for closed value sets that the domain models as enums, for example incident status, actor kind, event kind, approval decision, execution outcome, idempotency operation/state, evidence kind, recommendation, and diagnosis strategy.
- Keep enum labels aligned with domain `String()` values and HTTP wire values.
- Use `CHECK` constraints for scalar invariants that are not enum membership: positive versions, thresholds, non-negative replicas, timestamps ordering, distinct revisions, and failure-reason consistency.
- Use primary keys for domain IDs and unique constraints for one-to-one app invariants.
- Add foreign keys where they enforce real ownership or binding required by the workflow. Avoid cross-service or speculative foreign keys.
- Choose `ON DELETE` deliberately:
  - `CASCADE` for rows fully owned by an incident and meaningless without it.
  - `RESTRICT` for audit, approval, execution, proposal, or idempotency links that should not silently disappear.
- Avoid nullable columns unless the domain value is genuinely optional. If a nullable column is required for state-dependent data, enforce the state relationship with `CHECK`.
- Prefer forward-only, reviewable migrations. For an unmerged initial schema, edit the initial migration directly; after release, add a new migration instead.
- Down migrations must drop dependent constraints/tables before referenced tables and drop enum types last.

## Index Standards

- Start from actual query paths in code. Add indexes only for current reads, uniqueness, optimistic concurrency, or known workflow invariants.
- Do not add indexes for hypothetical dashboards, future search, broad filtering, or admin reports.
- Rely on primary-key and unique-constraint indexes; do not duplicate them with explicit indexes.
- Use composite indexes in the exact order used by lookup and ordering, for example `(incident_id, collected_at, id)` for evidence listing.
- Use partial unique indexes for state-scoped invariants, such as one active incident per deployment target.
- Prefer `UNIQUE (incident_id, sequence)` for ordered append-only audit timelines; add a separate non-unique index only if query plans show the unique index is insufficient.
- Before adding performance indexes after the schema is in use, verify with `EXPLAIN (ANALYZE, BUFFERS)` against representative data.

## DML And Transaction Standards

- Every blocking DB operation must receive a `context.Context`; if repository port methods do not take context, capture it in the transaction-scoped repository created by `UnitOfWork`.
- State mutation plus audit event insertion must happen in one database transaction.
- Use optimistic locking for incident state changes: `UPDATE ... WHERE id = $id AND version = $expected`; map zero affected rows to `domain.ErrVersionConflict`.
- For deterministic audit sequence generation, lock the parent incident row with `SELECT ... FOR UPDATE`, then compute `COALESCE(MAX(sequence), 0) + 1` inside the same transaction.
- Use parameter binding for all values. Never interpolate user-controlled values into SQL.
- Map `pgx.ErrNoRows`, unique violations, FK violations, and CHECK violations into typed/domain errors expected by callers.
- Validate persisted rows by rehydrating through domain constructors/snapshots; do not use unsafe field access, reflection hacks, or partial struct reconstruction.
- Keep transactions short: do not call external services while a DB transaction is open unless the application layer already explicitly models that boundary.

## Testing

- Add static migration tests for schema invariants that can regress cheaply, such as enum usage, required FKs, and absence of speculative indexes.
- Add PostgreSQL integration tests only against isolated disposable databases, preferably Testcontainers or a clearly named test DSN. Never truncate or mutate a developer's runtime database.
- Test repository behavior against the same contract as memory repositories: create/get/update, optimistic lock conflicts, event ordering, idempotency conflicts, proposal-hash approval binding, execution binding, and optional summary rows returning `domain.ErrNotFound`.
- Run:

```bash
GOCACHE=/tmp/rollback-guard-go-cache go test ./...
GOCACHE=/tmp/rollback-guard-go-cache GOLANGCI_LINT_CACHE=/tmp/rollback-guard-golangci-cache make lint
git diff --check
```
