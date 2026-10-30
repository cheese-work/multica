-- When the daemon confirmed it stopped a cancelled task's runner (its
-- cancel-ack, sent after the drain). The only automatic proof that a cancelled
-- run is no longer running; the stop gate opens on it and on nothing else.
-- NULL: no ack yet, or a task that was never cancelled.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS cancel_acked_at TIMESTAMPTZ;

-- Set when an armed superseded run (cancelled, failure_reason
-- 'superseded_head', no ack) is rewritten to any other status or reason before
-- the daemon confirmed the stop: /fail, a cleanup-unconfirmed report, a late
-- complete, any other writer. Keeps the run in the stop gate. A stop that is
-- not confirmed is not a confirmed stop.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS stop_unconfirmed_at TIMESTAMPTZ;

-- The guard lives with the row, not in one handler: many writers touch
-- status/failure_reason and each would otherwise have to remember it.
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
