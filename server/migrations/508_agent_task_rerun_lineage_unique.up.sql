-- CHE-485 follow-up: FindLiveRerunOfTask (server/pkg/db/queries/agent.sql) was
-- a preflight read with no backing constraint, so two concurrent first
-- admissions for the same (rerun_of_task_id, originator_user_id) could both
-- miss the read and both insert. This index makes "at most one live rerun per
-- source task per actor" a DB-enforced invariant; the service layer catches
-- the resulting 23505 and returns the winner instead of erroring.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_one_live_rerun_per_source_task_actor
    ON agent_task_queue (rerun_of_task_id, originator_user_id)
    WHERE rerun_of_task_id IS NOT NULL
      AND status IN ('queued', 'dispatched', 'running', 'waiting_local_directory');
