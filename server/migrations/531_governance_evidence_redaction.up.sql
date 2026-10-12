ALTER TABLE governance_evaluation
    ADD COLUMN IF NOT EXISTS redacted_at TIMESTAMPTZ;

ALTER TABLE governance_evaluation_source
    ADD COLUMN IF NOT EXISTS redacted_at TIMESTAMPTZ;

ALTER TABLE governance_attempt
    ADD COLUMN IF NOT EXISTS redacted_at TIMESTAMPTZ;
