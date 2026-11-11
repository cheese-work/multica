DROP TABLE IF EXISTS run_input;
ALTER TABLE agent_task_queue
    DROP CONSTRAINT IF EXISTS agent_task_queue_admission_state_check,
    DROP COLUMN IF EXISTS reservation_expires_at,
    DROP COLUMN IF EXISTS reservation_lease_id,
    DROP COLUMN IF EXISTS hold_reason,
    DROP COLUMN IF EXISTS admission_state;
