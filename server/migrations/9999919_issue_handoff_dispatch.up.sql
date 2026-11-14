SET LOCAL lock_timeout = '2s';

-- Write-ahead intent of a workflow handoff to an agent or squad (CHE-5486). The
-- status transaction writes it; dispatchHandoffIntent turns it into one run.
-- No foreign keys: the issue and task are validated in application code.
CREATE TABLE IF NOT EXISTS issue_handoff_dispatch (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    status_history_id uuid,
    target_type text NOT NULL,
    target_id uuid NOT NULL,
    actor_type text NOT NULL,
    actor_id uuid,
    source_task_id uuid,
    context_comment_ids uuid[] NOT NULL DEFAULT '{}',
    handoff_write text NOT NULL,
    state text NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'dispatched', 'skipped', 'failed')),
    task_id uuid,
    skip_reason text,
    attempts int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The run an intent started, in every task state. The unique index is its own
-- migration (9999916).
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS handoff_dispatch_id uuid;
