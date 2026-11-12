SET LOCAL lock_timeout = '2s';

-- CHE-4383 B4a: a System 2 labeller writes labels through the API. The row records
-- which task and agent wrote it; a correction points at the row it replaces.
-- No foreign keys: task_id, agent_id and supersedes are checked in application code.
ALTER TABLE system1_label DROP CONSTRAINT system1_label_source_check;
ALTER TABLE system1_label ADD CONSTRAINT system1_label_source_check CHECK (source IN ('human_answer', 'labeller'));
ALTER TABLE system1_label ADD COLUMN task_id uuid, ADD COLUMN agent_id uuid, ADD COLUMN supersedes uuid;
