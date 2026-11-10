# ADR: Transaction-scoped context for repositories

Status: accepted (R8.3)

## Context

`ports.UnitOfWork.WithinTransaction(ctx, fn)` hands `fn` a `Repositories` value.
27 of its 28 repository methods take no `context.Context`; the Postgres adapter
keeps the transaction's context in its struct and uses it for every statement.
`EventRepository.NextSequence` alone took a context and ignored it, which made
the port inconsistent.

## Decision

Repository methods do not take a `context.Context`. The context given to
`WithinTransaction` scopes the whole transaction: it is the only context the
adapters use, and cancelling it aborts in-flight statements and prevents the
commit. The stray parameter on `NextSequence` is removed (port, Postgres and
memory adapters, callers, tests). The contract is documented on the
`Repositories` and `UnitOfWork` types in `internal/ports/ports.go`.

Threading `ctx` through every method was rejected: it adds a parameter to about
28 methods and every caller, while a per-call context could not usefully differ
from the transaction's own, and could invite statements that outlive it.

## Consequences

- One rule: the transaction owns the context. No per-call deadlines inside a
  transaction; set them on the context passed to `WithinTransaction`.
- Repositories must not be used after `fn` returns.
- The memory adapter no longer checks the context in `NextSequence`; its
  `WithinTransaction` still refuses to commit on a cancelled context.
- If a repository call ever needs its own cancellation, revisit this ADR.
