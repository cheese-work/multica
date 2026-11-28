SET LOCAL lock_timeout = '500ms';
SET LOCAL statement_timeout = '600ms';

-- Writes one machine_online_event row for every committed online transition of
-- agent_runtime (CHE-9254). No EXCEPTION block: if the insert fails, the runtime
-- write fails with it, so an online transition commits if and only if its event
-- commits. The insert waits at most 500 ms for a lock on its own table; the
-- caller's lock_timeout is restored right after.
CREATE OR REPLACE FUNCTION machine_online_event_record() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    caller_lock_timeout text := current_setting('lock_timeout');
BEGIN
    PERFORM set_config('lock_timeout', '500ms', true);
    INSERT INTO machine_online_event (machine_owner_id, daemon_id)
    VALUES (COALESCE(NEW.owner_id, NEW.workspace_id), NEW.daemon_id);
    PERFORM set_config('lock_timeout', caller_lock_timeout, true);
    RETURN NULL;
END;
$$;

-- The trigger statements take SHARE ROW EXCLUSIVE on agent_runtime (writers
-- wait, readers do not) and are the last statements of the file, so the lock is
-- held for as little as possible. A heartbeat touch sets none of the listed
-- columns and a re-mark of an online row is skipped by WHEN.
CREATE OR REPLACE TRIGGER agent_runtime_online_event_ins
AFTER INSERT ON agent_runtime
FOR EACH ROW
WHEN (NEW.status = 'online' AND NEW.daemon_id IS NOT NULL)
EXECUTE FUNCTION machine_online_event_record();

CREATE OR REPLACE TRIGGER agent_runtime_online_event_upd
AFTER UPDATE OF status, owner_id, daemon_id ON agent_runtime
FOR EACH ROW
WHEN (
    NEW.status = 'online' AND NEW.daemon_id IS NOT NULL AND (
        OLD.status IS DISTINCT FROM 'online'
        OR COALESCE(OLD.owner_id, OLD.workspace_id) IS DISTINCT FROM COALESCE(NEW.owner_id, NEW.workspace_id)
        OR OLD.daemon_id IS DISTINCT FROM NEW.daemon_id
    )
)
EXECUTE FUNCTION machine_online_event_record();
