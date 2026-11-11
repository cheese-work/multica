-- CHE-5213: a comment that coalesces into a queued run (any code path) is a
-- trigger of that run. Record each one as a held run input with its stable
-- event id so a pause, a WIP hold or a failed run never drops it. INSERT is
-- covered by the enqueue funnel in the service layer.
CREATE OR REPLACE FUNCTION run_input_from_coalesced_comments() RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'queued' OR NEW.issue_id IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload)
    SELECT a.workspace_id, NEW.issue_id, NEW.agent_id, 'comment:' || c.id::text, 'comment',
           jsonb_build_object('comment_id', c.id::text, 'task_id', NEW.id::text,
                              'content', left(COALESCE(cm.content, ''), 2000))
    FROM agent a,
         unnest(NEW.coalesced_comment_ids || CASE WHEN NEW.trigger_comment_id IS NULL THEN '{}'::uuid[] ELSE ARRAY[NEW.trigger_comment_id] END) AS c(id)
         LEFT JOIN comment cm ON cm.id = c.id
    WHERE a.id = NEW.agent_id
    ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_task_queue_run_input_comments
AFTER UPDATE OF coalesced_comment_ids, trigger_comment_id ON agent_task_queue
FOR EACH ROW EXECUTE FUNCTION run_input_from_coalesced_comments();
