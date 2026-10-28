-- The pull request a review run was admitted against. With the run's head_sha
-- stamp and workflow_step it identifies "this PR's review at this step", so a
-- new head can supersede the run and one replacement can stand in for it.
-- NULL: not a review run, or the issue had no linked PR at that head.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS pull_request_id UUID;

-- Claim marker on a cancelled superseded_head run: its replacement was
-- enqueued. Set once per (issue, PR, step) group, after the stop gate opened.
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS replacement_released_at TIMESTAMPTZ;
