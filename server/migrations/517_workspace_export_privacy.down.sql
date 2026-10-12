ALTER TABLE workspace
    DROP CONSTRAINT IF EXISTS workspace_export_redaction_mode_check,
    DROP CONSTRAINT IF EXISTS workspace_export_manifest_retention_days_check;

ALTER TABLE workspace
    DROP COLUMN IF EXISTS export_redaction_mode,
    DROP COLUMN IF EXISTS export_manifest_retention_days;
