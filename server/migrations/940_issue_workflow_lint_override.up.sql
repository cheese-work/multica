ALTER TABLE issue_workflow
    ADD COLUMN IF NOT EXISTS lint_override_reason text,
    ADD COLUMN IF NOT EXISTS lint_overridden_at timestamptz,
    ADD COLUMN IF NOT EXISTS lint_findings jsonb NOT NULL DEFAULT '[]'::jsonb;
