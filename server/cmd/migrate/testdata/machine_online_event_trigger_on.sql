SET LOCAL lock_timeout = '500ms';
SET LOCAL statement_timeout = '600ms';

-- Rollback step 4 (CHE-9254): transitions during the gap were not recorded, so
-- a new generation changes every machine's id before the triggers record again.
-- Both statements run in one transaction; if the update fails, the triggers
-- stay disabled. Ships as a forward migration only when a rollback needs it.
UPDATE machine_online_event_generation SET generation = gen_random_uuid();

ALTER TABLE agent_runtime
    ENABLE TRIGGER agent_runtime_online_event_ins,
    ENABLE TRIGGER agent_runtime_online_event_upd;
