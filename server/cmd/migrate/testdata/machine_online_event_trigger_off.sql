SET LOCAL lock_timeout = '500ms';
SET LOCAL statement_timeout = '600ms';

-- Rollback step 3 (CHE-9254): stop recording online transitions. Reads neither
-- event table, so it works while the event insert fails. Ships as a forward
-- migration only when a rollback needs it.
ALTER TABLE agent_runtime
    DISABLE TRIGGER agent_runtime_online_event_ins,
    DISABLE TRIGGER agent_runtime_online_event_upd;
