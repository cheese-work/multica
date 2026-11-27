DELETE FROM plugin_invocation WHERE trigger = 'guard';

ALTER TABLE plugin_invocation DROP CONSTRAINT IF EXISTS plugin_invocation_trigger_check;
ALTER TABLE plugin_invocation ADD CONSTRAINT plugin_invocation_trigger_check
    CHECK (trigger IN ('ui', 'manual', 'event', 'agent', 'schedule')) NOT VALID;
-- Migration 9999973 validates the wider constraint and its own rollback is a
-- no-op, so restore the validated state here to match the pre-guard schema.
ALTER TABLE plugin_invocation VALIDATE CONSTRAINT plugin_invocation_trigger_check;
