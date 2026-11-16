ALTER TABLE agent
    ADD COLUMN IF NOT EXISTS scheduling_paused_reason TEXT;
