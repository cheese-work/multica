SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

-- Chat capability rows have no issue; an older server reads issue_id as NOT NULL.
DELETE FROM task_supplement_capability WHERE issue_id IS NULL;
ALTER TABLE task_supplement_capability
    DROP COLUMN IF EXISTS chat_session_id,
    ALTER COLUMN issue_id SET NOT NULL;

DROP TABLE IF EXISTS chat_task_supplement;
