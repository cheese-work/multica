-- Server-side machine pause (CHE-2430). A row means the machine
-- (machine_owner_id, daemon_id) takes no new task claims. A daemon_id is picked
-- by the client at registration, so on its own it proves nothing: the machine is
-- the pair, where machine_owner_id is the runtime's owner (its workspace when a
-- runtime has no owner). One owner's machine joined to several workspaces is one
-- row; another owner who copies the daemon_id is another machine. It lives on
-- the server so it survives a daemon restart and gates old daemon versions too.
-- Running tasks finish; resume deletes the row. workspace_id is where the pause
-- was set, kept for display and teardown. No foreign keys.
CREATE TABLE daemon_pause (
    machine_owner_id UUID NOT NULL,
    daemon_id TEXT NOT NULL,
    workspace_id UUID NOT NULL,
    reason TEXT NOT NULL CHECK (reason IN ('maintenance', 'incident', 'suspended')),
    paused_by UUID,
    paused_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (machine_owner_id, daemon_id)
);
