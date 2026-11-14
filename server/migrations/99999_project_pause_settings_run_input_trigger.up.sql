-- A paused project keeps accepting runs (CHE-5213): the run is held at claim and
-- starts after unpause. Only a cancelled or completed project refuses a write.
CREATE OR REPLACE FUNCTION lock_active_project(p_project_id uuid, p_workspace_id uuid)
RETURNS boolean LANGUAGE plpgsql AS $$
DECLARE project_status text;
BEGIN
    IF p_project_id IS NULL THEN RETURN TRUE; END IF;
    PERFORM id FROM workspace WHERE id = p_workspace_id FOR KEY SHARE;
    SELECT status INTO project_status FROM project
    WHERE id = p_project_id AND workspace_id = p_workspace_id FOR SHARE;
    IF project_status IN ('cancelled', 'completed') THEN
        RAISE EXCEPTION 'project_inactive' USING ERRCODE = 'P0001';
    END IF;
    RETURN TRUE;
END;
$$;

-- Generic per-project settings (CHE-5213). Key `wip_limit` (integer >= 1) caps
-- the runs a project has dispatched, running or reserved at once; absent means
-- unlimited. One JSON column, not one column per setting.
ALTER TABLE project ADD COLUMN IF NOT EXISTS settings jsonb NOT NULL DEFAULT '{}'::jsonb;

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
