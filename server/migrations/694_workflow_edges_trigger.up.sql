-- Step entry (CHE-2152). Every status write, from any writer, replaces the
-- issue's workflow_edges watch: leaving a step drops it, and entering a step
-- whose workflow has fact-guarded edges starts one, due at once. Its created_at
-- is the step-entry time that edge timeouts count from. The service evaluates
-- the row; this only keeps it in step with the status.
SET LOCAL lock_timeout = '2s';

CREATE OR REPLACE FUNCTION sync_issue_workflow_edges() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM issue_wakeup WHERE issue_id = NEW.id AND system_rule = 'workflow_edges';
    -- The workflow an issue follows is its override, else its project's, else
    -- the workspace default. Only a step of that workflow with an edge a stored
    -- fact can decide (not agent_done / agent_verdict) starts a watch.
    IF EXISTS (
        SELECT 1
        FROM issue_workflow wf
        WHERE wf.workspace_id = NEW.workspace_id
          AND (wf.id = NEW.workflow_id
               OR (NEW.workflow_id IS NULL AND NEW.project_id IS NOT NULL AND (
                    wf.id = (SELECT p.workflow_id FROM project p WHERE p.id = NEW.project_id)
                    OR (wf.is_default AND (SELECT p.workflow_id FROM project p WHERE p.id = NEW.project_id) IS NULL))))
          AND jsonb_path_exists(
                wf.steps,
                '$[*] ? (@.status_key == $status) . edges[*] ? (!(@.guard starts with "agent_done") && !(@.guard starts with "agent_verdict"))',
                jsonb_build_object('status', NEW.status))
    ) THEN
        INSERT INTO issue_wakeup(id, workspace_id, issue_id, instruction, kind, mode, event_types, timezone,
                                 condition_state, enabled, system_rule, next_fire_at, created_at, updated_at)
        VALUES (gen_random_uuid(), NEW.workspace_id, NEW.id, '', 'at', 'once', '{}', 'UTC',
                NEW.status, true, 'workflow_edges', now(), now(), now())
        ON CONFLICT (issue_id, system_rule) WHERE system_rule IS NOT NULL DO NOTHING;
    END IF;
    RETURN NULL;
END $$;

CREATE TRIGGER issue_workflow_edges_insert
    AFTER INSERT ON issue
    FOR EACH ROW EXECUTE FUNCTION sync_issue_workflow_edges();

CREATE TRIGGER issue_workflow_edges_update
    AFTER UPDATE OF status ON issue
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION sync_issue_workflow_edges();

-- A run-budget block names its reason in the issue metadata; leaving blocked
-- ends that reason.
CREATE OR REPLACE FUNCTION clear_issue_run_budget_reason() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.metadata = NEW.metadata - 'blocked_reason';
    RETURN NEW;
END $$;

CREATE TRIGGER issue_run_budget_reason_clear
    BEFORE UPDATE OF status ON issue
    FOR EACH ROW WHEN (OLD.status = 'blocked' AND NEW.status IS DISTINCT FROM 'blocked'
                       AND OLD.metadata ->> 'blocked_reason' = 'run-budget')
    EXECUTE FUNCTION clear_issue_run_budget_reason();
