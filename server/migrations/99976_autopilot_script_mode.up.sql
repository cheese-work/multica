-- CHE-5090 (CHE-4678 S9 P1): autopilot execution_mode "script". One JSONB
-- settings field holds {"name", "args"}; the handler validates it. No column
-- per setting. NULL for every other mode.
ALTER TABLE autopilot ADD COLUMN IF NOT EXISTS script_config JSONB;
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_execution_mode_check;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_execution_mode_check
    CHECK (execution_mode IN ('create_issue', 'run_only', 'script'));
