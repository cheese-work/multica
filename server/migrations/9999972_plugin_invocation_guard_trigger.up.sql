-- A guard hook is called synchronously before a write, and its call is recorded
-- in plugin_invocation like every other trigger. Add the `guard` trigger to the
-- constraint as NOT VALID so no existing row is scanned under a lock; the
-- companion migration validates it (same pattern as 399/402).
ALTER TABLE plugin_invocation DROP CONSTRAINT IF EXISTS plugin_invocation_trigger_check;
ALTER TABLE plugin_invocation ADD CONSTRAINT plugin_invocation_trigger_check
    CHECK (trigger IN ('ui', 'manual', 'event', 'agent', 'schedule', 'guard')) NOT VALID;
