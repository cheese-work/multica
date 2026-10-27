SET LOCAL lock_timeout = '2s';

DELETE FROM decision_answer WHERE answered_by_task_id IS NULL;
ALTER TABLE decision_answer ALTER COLUMN answered_by_task_id SET NOT NULL;
ALTER TABLE decision_request DROP COLUMN IF EXISTS options;
