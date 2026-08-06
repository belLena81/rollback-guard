DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM investigation_results
        WHERE strategy::text <> 'rules'
           OR requested_strategy::text <> 'rules'
    ) THEN
        RAISE EXCEPTION 'cannot remove AI diagnosis migration while model-backed results exist';
    END IF;
END
$$;

ALTER TABLE investigation_results
    DROP CONSTRAINT investigation_results_provenance_check,
    DROP COLUMN fallback_reason,
    DROP COLUMN output_tokens,
    DROP COLUMN input_tokens,
    DROP COLUMN prompt_version,
    DROP COLUMN model,
    DROP COLUMN requested_strategy;

DROP TYPE diagnosis_fallback_reason;

ALTER TYPE diagnosis_strategy RENAME TO diagnosis_strategy_phase5;

CREATE TYPE diagnosis_strategy AS ENUM ('rules');

ALTER TABLE investigation_results
    ALTER COLUMN strategy TYPE diagnosis_strategy
    USING strategy::text::diagnosis_strategy;

DROP TYPE diagnosis_strategy_phase5;
