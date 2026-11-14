SET LOCAL lock_timeout = '2s';

-- Agent outcome of one chat turn (CHE-4149, plan r4 T6-T9). One row per task: the first outcome
-- the task reports, or 'unclassified' written when the task completes without one. No foreign
-- keys (repo rule): task, workspace, session, chain and user ids are soft references; task_id is the key.
-- workspace_id lets workspace delete remove the rows even after the root session was hard-deleted.
CREATE TABLE IF NOT EXISTS chat_turn_outcome (
    task_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    chat_session_id uuid NOT NULL,
    chain_root_session_id uuid NOT NULL,
    user_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('question', 'done', 'info', 'unclassified')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
