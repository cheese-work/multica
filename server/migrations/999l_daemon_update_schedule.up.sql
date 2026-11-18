-- Per-machine update schedule (CHE-5465). One row per machine (machine_owner_id,
-- daemon_id), the same key as daemon_pause. member_setting and workspace.settings
-- cannot hold it: both are keyed by workspace, and one machine serves several
-- workspaces. The cycle driver (later children) also reads every enabled row on
-- one scan, which a per-workspace JSON blob cannot answer. No foreign keys.
-- window_minutes 30 is the hard cap on how long a maintenance window may hold claims.
CREATE TABLE daemon_update_schedule (
    machine_owner_id UUID NOT NULL,
    daemon_id TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT false,
    cron TEXT NOT NULL,
    timezone TEXT NOT NULL DEFAULT 'Asia/Ho_Chi_Minh',
    window_minutes INTEGER NOT NULL DEFAULT 30 CHECK (window_minutes BETWEEN 10 AND 30),
    updated_by UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (machine_owner_id, daemon_id)
);
