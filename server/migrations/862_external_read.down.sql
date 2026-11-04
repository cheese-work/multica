DROP TRIGGER IF EXISTS comment_inherit_external_read ON comment;
DROP FUNCTION IF EXISTS comment_inherit_external_read();
ALTER TABLE issue DROP COLUMN IF EXISTS description_external_read;
ALTER TABLE attachment DROP COLUMN IF EXISTS external_read;
ALTER TABLE comment DROP COLUMN IF EXISTS external_read;
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS external_read;
