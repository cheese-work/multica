-- CHE-2298 c1: a PR merged or closed during a run cancels the work that only
-- serves that PR. Those cancels arm the stop gate like a superseded head, so the
-- trigger that keeps an unconfirmed stop in the gate covers their reasons too.
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
          AND OLD.failure_reason IN ('superseded_head', 'pr_merged', 'pr_closed')
          AND OLD.dispatched_at IS NOT NULL
          AND OLD.cancel_acked_at IS NULL
          AND NEW.cancel_acked_at IS NULL
          AND (NEW.status IS DISTINCT FROM OLD.status
               OR NEW.failure_reason IS DISTINCT FROM OLD.failure_reason))
    EXECUTE FUNCTION agent_task_keep_stop_unconfirmed();
