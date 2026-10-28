-- Typed PR-event capability (CHE-2298 a2). Its own table: task_supplement_capability
-- keeps one row per task and task-supplement-v1, so member steering and PR-event
-- delivery negotiate independently on the same task. No foreign keys.
CREATE TABLE IF NOT EXISTS task_pr_event_capability (
    task_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    capability TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
