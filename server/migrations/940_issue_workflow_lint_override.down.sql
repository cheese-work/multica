ALTER TABLE issue_workflow
    DROP COLUMN IF EXISTS lint_override_reason,
    DROP COLUMN IF EXISTS lint_overridden_at;
