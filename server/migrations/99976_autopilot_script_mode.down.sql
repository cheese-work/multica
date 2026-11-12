-- Reversible while no script autopilot exists. A script autopilot has no
-- create_issue/run_only equivalent, so the down refuses instead of rewriting it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM autopilot WHERE execution_mode = 'script') THEN
        RAISE EXCEPTION 'cannot roll back 99965: script-mode autopilots exist; delete or archive them first';
    END IF;
END $$;
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_execution_mode_check;
ALTER TABLE autopilot ADD CONSTRAINT autopilot_execution_mode_check
    CHECK (execution_mode IN ('create_issue', 'run_only'));
ALTER TABLE autopilot DROP COLUMN IF EXISTS script_config;
