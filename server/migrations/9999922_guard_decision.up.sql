-- CHE-5207: one row per tool-guard decision, uploaded by the daemon when a run
-- ends (CHE-5194). The settings page and `multica guard log|stats` read it.
-- No foreign keys: a decision outlives a deleted task or issue, and workspace
-- deletion is not worth a cascade over a counter table.
SET LOCAL lock_timeout = '2s';

CREATE TABLE IF NOT EXISTS guard_decision (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    task_id      UUID NOT NULL,
    -- Position in the run's decision log; a retried upload cannot add a row twice.
    seq          INTEGER NOT NULL,
    decided_at   TIMESTAMPTZ NOT NULL,
    plugin       TEXT NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('pass', 'fixed', 'refused', 'plugin-error', 'shadow-hit')),
    tool         TEXT NOT NULL DEFAULT '',
    subcommand   TEXT NOT NULL DEFAULT '',
    line         TEXT NOT NULL DEFAULT '',
    issue_key    TEXT NOT NULL DEFAULT '',
    note         TEXT NOT NULL DEFAULT '',
    runtime      TEXT NOT NULL DEFAULT '',
    UNIQUE (task_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_guard_decision_workspace_at
    ON guard_decision (workspace_id, decided_at DESC, id DESC);
