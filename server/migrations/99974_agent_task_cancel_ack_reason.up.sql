-- CHE-3114 part 2: how cancel_acked_at was established. NULL: the daemon's own
-- per-task cancel-ack. 'daemon_reconcile': the claiming runtime's live task
-- set no longer held the run. Audit only; the stop gate reads cancel_acked_at.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS cancel_ack_reason TEXT;
