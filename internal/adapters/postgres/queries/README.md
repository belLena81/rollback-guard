# Postgres Queries

Phase 2 keeps SQL beside the repository adapter so queries remain reviewable.
The `sqlc.yaml` file is present to support generated accessors once query
volume warrants generation; the current adapter uses `pgx` directly to avoid
requiring generator binaries for the default build.
