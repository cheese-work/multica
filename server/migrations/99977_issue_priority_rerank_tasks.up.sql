-- CHE-5118: agent_task_queue.priority is stamped at enqueue, so an issue raised
-- to urgent kept its old rank and waited behind older medium work. A trigger
-- covers every writer of issue.priority. Mapping mirrors priorityToInt in
-- service/task.go. Only tasks that have not started are re-ranked.
CREATE FUNCTION issue_priority_rerank_tasks() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 UPDATE agent_task_queue
 SET priority = CASE NEW.priority WHEN 'urgent' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END
 WHERE issue_id = NEW.id AND status IN ('queued', 'deferred');
 RETURN NULL;
END $$;

CREATE TRIGGER issue_priority_rerank_tasks
AFTER UPDATE OF priority ON issue
FOR EACH ROW WHEN (OLD.priority IS DISTINCT FROM NEW.priority)
EXECUTE FUNCTION issue_priority_rerank_tasks();
