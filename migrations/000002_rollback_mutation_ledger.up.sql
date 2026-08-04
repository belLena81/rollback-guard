CREATE TYPE rollback_mutation_state AS ENUM (
    'pending',
    'completed'
);

ALTER TABLE idempotency_keys
    ADD CONSTRAINT idempotency_keys_key_request_hash_unique UNIQUE (key, request_hash);

CREATE TABLE rollback_mutations (
    idempotency_key text PRIMARY KEY,
    proposal_hash text NOT NULL,
    execution_id text NOT NULL UNIQUE,
    from_revision text NOT NULL,
    to_revision text NOT NULL,
    state rollback_mutation_state NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    already_applied boolean,
    CHECK (from_revision <> to_revision),
    CHECK (
        (state = 'pending' AND completed_at IS NULL AND already_applied IS NULL)
        OR
        (state = 'completed' AND completed_at IS NOT NULL AND completed_at >= started_at AND already_applied IS NOT NULL)
    ),
    FOREIGN KEY (idempotency_key, proposal_hash)
        REFERENCES idempotency_keys(key, request_hash)
        ON DELETE RESTRICT
);
