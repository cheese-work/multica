-- Server-side machine pause (CHE-2430). A row means the daemon identified by
-- (workspace_id, daemon_id) takes no new task claims. It lives on the server so
-- it survives a daemon restart and gates old daemon versions too. Running tasks
-- finish; resume deletes the row.
CREATE TABLE daemon_pause (
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    daemon_id TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('maintenance', 'incident', 'suspended')),
    paused_by UUID REFERENCES "user"(id) ON DELETE SET NULL,
    paused_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, daemon_id)
);
