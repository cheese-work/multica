-- Scheduled daemon update cycle (CHE-5466, parent CHE-5463). A cycle owns the
-- machine pause for its window: the pause row carries cycle_id and expires_at,
-- and a person's pause or resume cancels the cycle. One open cycle per machine
-- (the partial unique index, own migration). No foreign keys.
CREATE TABLE IF NOT EXISTS daemon_update_cycle (
    id UUID PRIMARY KEY,
    machine_owner_id UUID NOT NULL,
    daemon_id TEXT NOT NULL,
    fire_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deadline_at TIMESTAMPTZ NOT NULL,
    state TEXT NOT NULL,
    attempt INT NOT NULL DEFAULT 0,
    update_request_id TEXT,
    runtime_id UUID,
    from_commit TEXT,
    target_commit TEXT,
    cancel_requested TEXT,
    request_status TEXT,
    request_reason TEXT,
    request_output TEXT,
    request_error TEXT,
    request_reported_at TIMESTAMPTZ,
    outcome TEXT,
    outcome_detail TEXT,
    -- NULL while the cycle is open.
    outcome_source TEXT CHECK (outcome_source IN ('server', 'daemon_result', 'commit', 'none')),
    notes TEXT[] NOT NULL DEFAULT '{}',
    live_runs_at_end INT,
    finished_at TIMESTAMPTZ,
    -- Claims stay blocked at most 30 min (Cheese, 13 Nov 2026).
    CONSTRAINT daemon_update_cycle_deadline_check CHECK (deadline_at <= started_at + interval '30 minutes')
);

ALTER TABLE daemon_pause ADD COLUMN IF NOT EXISTS cycle_id UUID;
ALTER TABLE daemon_pause ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
ALTER TABLE daemon_pause DROP CONSTRAINT IF EXISTS daemon_pause_reason_check;
ALTER TABLE daemon_pause ADD CONSTRAINT daemon_pause_reason_check
    CHECK (reason IN ('maintenance', 'incident', 'suspended', 'scheduled_update'));
