SET LOCAL lock_timeout = '2s';

ALTER TABLE issue_workflow
    DROP COLUMN IF EXISTS max_runs_per_issue,
    DROP COLUMN IF EXISTS include_child_prs,
    DROP COLUMN IF EXISTS run_budget_since;
