-- When the daemon confirmed it stopped a cancelled task's runner (its
-- cancel-ack, sent after the drain). The only automatic proof that a cancelled
-- run is no longer running; the stop gate opens on it and on nothing else.
-- NULL: no ack yet, or a task that was never cancelled.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS cancel_acked_at TIMESTAMPTZ;
-- Set when an armed superseded run reported its cleanup unconfirmed. That
-- report rewrites the row to failed/cleanup_unconfirmed, which would drop the
-- run out of the gate; this keeps it armed. A stop that is not confirmed is
-- not a confirmed stop.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS stop_unconfirmed_at TIMESTAMPTZ;
