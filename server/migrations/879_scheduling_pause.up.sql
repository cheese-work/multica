ALTER TABLE agent
    ADD COLUMN IF NOT EXISTS scheduling_paused_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS scheduling_paused_by UUID;

ALTER TABLE workspace
    ADD COLUMN IF NOT EXISTS scheduling_paused_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS scheduling_paused_by UUID;
