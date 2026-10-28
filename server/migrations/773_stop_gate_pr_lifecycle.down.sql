CREATE OR REPLACE FUNCTION agent_task_keep_stop_unconfirmed() RETURNS trigger AS $$
BEGIN
    NEW.stop_unconfirmed_at := COALESCE(OLD.stop_unconfirmed_at, now());
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS agent_task_keep_stop_unconfirmed ON agent_task_queue;
CREATE TRIGGER agent_task_keep_stop_unconfirmed
    BEFORE UPDATE OF status, failure_reason ON agent_task_queue
    FOR EACH ROW
    WHEN (OLD.status = 'cancelled'
          AND OLD.failure_reason = 'superseded_head'
          AND OLD.dispatched_at IS NOT NULL
          AND OLD.cancel_acked_at IS NULL
          AND NEW.cancel_acked_at IS NULL
          AND (NEW.status IS DISTINCT FROM OLD.status
               OR NEW.failure_reason IS DISTINCT FROM OLD.failure_reason))
    EXECUTE FUNCTION agent_task_keep_stop_unconfirmed();

ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS stop_hold_issue_wide;
