-- Workflow-level run rules (CHE-2152): a cap on runs per issue, whether a
-- parent's PR guards also read its children's PRs, and the instant the budget
-- starts counting so issues already in flight begin with a fresh budget.
SET LOCAL lock_timeout = '2s';

ALTER TABLE issue_workflow
    ADD COLUMN IF NOT EXISTS max_runs_per_issue integer CHECK (max_runs_per_issue BETWEEN 1 AND 1000),
    ADD COLUMN IF NOT EXISTS include_child_prs boolean NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS run_budget_since timestamptz NOT NULL DEFAULT now();
