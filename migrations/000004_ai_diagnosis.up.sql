ALTER TYPE diagnosis_strategy RENAME TO diagnosis_strategy_v1;

CREATE TYPE diagnosis_strategy AS ENUM (
    'rules',
    'llm',
    'hybrid'
);

ALTER TABLE investigation_results
    ALTER COLUMN strategy TYPE diagnosis_strategy
    USING strategy::text::diagnosis_strategy;

DROP TYPE diagnosis_strategy_v1;

CREATE TYPE diagnosis_fallback_reason AS ENUM (
    'provider_error',
    'provider_timeout',
    'provider_refusal',
    'invalid_output'
);

ALTER TABLE investigation_results
    ADD COLUMN requested_strategy diagnosis_strategy NOT NULL DEFAULT 'rules',
    ADD COLUMN model text NOT NULL DEFAULT '',
    ADD COLUMN prompt_version text NOT NULL DEFAULT '',
    ADD COLUMN input_tokens bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    ADD COLUMN output_tokens bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    ADD COLUMN fallback_reason diagnosis_fallback_reason,
    ADD CONSTRAINT investigation_results_provenance_check CHECK (
        (
            strategy = 'rules'
            AND requested_strategy = 'rules'
            AND model = ''
            AND prompt_version = ''
            AND input_tokens = 0
            AND output_tokens = 0
            AND fallback_reason IS NULL
        )
        OR
        (
            strategy = 'rules'
            AND requested_strategy IN ('llm', 'hybrid')
            AND btrim(model) <> ''
            AND btrim(prompt_version) <> ''
            AND fallback_reason IS NOT NULL
        )
        OR
        (
            strategy IN ('llm', 'hybrid')
            AND requested_strategy = strategy
            AND btrim(model) <> ''
            AND btrim(prompt_version) <> ''
            AND fallback_reason IS NULL
        )
    );

ALTER TABLE investigation_results
    ALTER COLUMN requested_strategy DROP DEFAULT,
    ALTER COLUMN model DROP DEFAULT,
    ALTER COLUMN prompt_version DROP DEFAULT,
    ALTER COLUMN input_tokens DROP DEFAULT,
    ALTER COLUMN output_tokens DROP DEFAULT;
