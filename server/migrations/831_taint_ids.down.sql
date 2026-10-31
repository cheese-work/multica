DROP TRIGGER IF EXISTS wakeup_receipt_inherit_taint ON issue_wakeup_receipt;
DROP FUNCTION IF EXISTS wakeup_receipt_inherit_taint();
ALTER TABLE issue_wakeup_receipt DROP COLUMN IF EXISTS taint_ids;
DROP TRIGGER IF EXISTS agent_task_inherit_taint ON agent_task_queue;
DROP TRIGGER IF EXISTS comment_inherit_taint ON comment;
DROP FUNCTION IF EXISTS agent_task_inherit_taint();
DROP FUNCTION IF EXISTS comment_inherit_taint();
ALTER TABLE agent_task_queue DROP COLUMN IF EXISTS taint_ids;
ALTER TABLE comment DROP COLUMN IF EXISTS taint_ids;
