SET LOCAL lock_timeout = '2s';

-- How the issue was made: dialog, agent or cli (CHE-6098). Server-set at create;
-- NULL for issues made before this column existed.
ALTER TABLE issue ADD COLUMN IF NOT EXISTS created_via text;
