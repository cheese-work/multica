SET LOCAL lock_timeout = '2s';

-- A member's login lease cannot keep the NOT NULL columns, so those rows go.
DELETE FROM browser_lease WHERE class = 'H' AND task_id IS NULL;
ALTER TABLE browser_lease DROP CONSTRAINT IF EXISTS browser_lease_holder_by_class;
ALTER TABLE browser_lease
    ALTER COLUMN agent_id SET NOT NULL,
    ALTER COLUMN task_id SET NOT NULL,
    ALTER COLUMN issue_id SET NOT NULL;
