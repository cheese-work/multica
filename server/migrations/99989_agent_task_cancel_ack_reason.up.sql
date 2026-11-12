-- Why cancel_acked_at was set when it was not a proof-bearing daemon ack
-- (CHE-3114): 'daemon_reconcile' = the claiming runtime's daemon reported a live
-- task set that no longer holds the run. NULL: acked by the daemon itself, or
-- not acked.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS cancel_ack_reason TEXT;
