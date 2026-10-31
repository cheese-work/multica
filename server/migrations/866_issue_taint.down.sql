DROP TRIGGER IF EXISTS agent_task_issue_taint_ins ON agent_task_queue;
DROP TRIGGER IF EXISTS agent_task_issue_taint_upd ON agent_task_queue;
DROP TRIGGER IF EXISTS comment_issue_taint_ins ON comment;
DROP TRIGGER IF EXISTS comment_issue_taint_upd ON comment;
DROP FUNCTION IF EXISTS issue_taint_from_row();
DROP FUNCTION IF EXISTS issue_taint_note(uuid, text[]);
DROP TABLE IF EXISTS issue_taint_clear;
DROP TABLE IF EXISTS issue_taint;
