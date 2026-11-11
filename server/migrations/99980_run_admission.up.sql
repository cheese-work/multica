-- Dispatch-time admission (CHE-5213, slice G0 of CHE-5167).
--
-- A run keeps status 'queued' while it is held or reserved: every reader that
-- treats 'queued' as pending (coalescing, the one-pending-run unique index,
-- cancel paths, the run list) stays correct. The admission state sits beside the
-- status:
--   held     - not claimable; waits for a release event
--   reserved - claimable by one worker until reservation_expires_at; counts
--              against the project's WIP limit
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS admission_state text,
    ADD COLUMN IF NOT EXISTS hold_reason text,
    ADD COLUMN IF NOT EXISTS reservation_lease_id uuid,
    ADD COLUMN IF NOT EXISTS reservation_expires_at timestamptz;

ALTER TABLE agent_task_queue
    ADD CONSTRAINT agent_task_queue_admission_state_check
    CHECK (admission_state IS NULL OR admission_state IN ('held', 'reserved')) NOT VALID;

-- One row per trigger. The key is the trigger's stable event id, so only a
-- retry of the same event is deduplicated.
CREATE TABLE IF NOT EXISTS run_input (
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    event_id text NOT NULL,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    kind text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    state text NOT NULL DEFAULT 'held'
        CHECK (state IN ('held', 'claimed', 'delivered', 'parked', 'discarded')),
    task_id uuid,
    redelivery_of_task_id uuid,
    park_reason text,
    failure_class text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (issue_id, agent_id, event_id)
);
