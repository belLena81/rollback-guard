# Postgres Queries

Phase 2 keeps SQL beside the repository adapter so queries remain reviewable.
The adapter uses `pgx` directly; no code is generated. Generated accessors
(for example with sqlc) can be introduced later if query volume warrants it.
