-- Ask repair (CHE-3345 C7). One row per recovery root: the first guard event of
-- a chain, whose id is the root id. A repair task carries its root, and a guard
-- event it produces inherits that root (root_id). The row admits one repair
-- task (repair_task_id) and commits with it, so a restart finds the row and
-- enqueues nothing. A refused or unfinished repair sets state to `failed` with
-- a reason; the agent that produced the event owns the failure. Unique
-- indexes are the next migrations. No foreign keys (repo rule).
CREATE TABLE IF NOT EXISTS ask_repair_root (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    repair_task_id uuid,
    state text NOT NULL DEFAULT 'admitted' CHECK (state IN ('admitted', 'settled', 'failed')),
    failure_reason text,
    failure_comment_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    settled_at timestamptz,
    failed_at timestamptz
);
ALTER TABLE ask_guard_event
    ADD COLUMN IF NOT EXISTS root_id uuid,
    ADD COLUMN IF NOT EXISTS repair_handled_at timestamptz;
