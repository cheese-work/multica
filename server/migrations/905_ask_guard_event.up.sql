-- Ask guard events (CHE-3345): one row per operation the guard hit. Each row
-- keeps the workspace mode at creation (`warn` or `block`), so a later mode
-- switch never changes what an earlier event means. The operation key is
-- (task_id, operation_kind, payload_sha256, mode), so a retry after a switch to
-- block gets its own block event beside the earlier warn event. Its unique
-- index is the next migration. No foreign keys (repo rule).
CREATE TABLE IF NOT EXISTS ask_guard_event (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    task_id uuid NOT NULL,
    operation_kind text NOT NULL,
    payload_sha256 text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('warn', 'block')),
    created_at timestamptz NOT NULL DEFAULT now()
);
