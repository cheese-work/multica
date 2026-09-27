ALTER TABLE governance_attempt
    DROP COLUMN IF EXISTS redacted_at;

ALTER TABLE governance_evaluation_source
    DROP COLUMN IF EXISTS redacted_at;

ALTER TABLE governance_evaluation
    DROP COLUMN IF EXISTS redacted_at;
