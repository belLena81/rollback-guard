DROP TABLE IF EXISTS rollback_mutations;

ALTER TABLE idempotency_keys
    DROP CONSTRAINT IF EXISTS idempotency_keys_key_request_hash_unique;

DROP TYPE IF EXISTS rollback_mutation_state;
