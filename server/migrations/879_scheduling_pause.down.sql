ALTER TABLE workspace
    DROP COLUMN IF EXISTS scheduling_paused_by,
    DROP COLUMN IF EXISTS scheduling_paused_at;

ALTER TABLE agent
    DROP COLUMN IF EXISTS scheduling_paused_by,
    DROP COLUMN IF EXISTS scheduling_paused_at;
