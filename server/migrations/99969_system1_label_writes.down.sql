DELETE FROM system1_label WHERE source = 'labeller';
ALTER TABLE system1_label DROP COLUMN IF EXISTS supersedes, DROP COLUMN IF EXISTS agent_id, DROP COLUMN IF EXISTS task_id;
ALTER TABLE system1_label DROP CONSTRAINT IF EXISTS system1_label_source_check;
ALTER TABLE system1_label ADD CONSTRAINT system1_label_source_check CHECK (source IN ('human_answer'));
