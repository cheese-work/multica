ALTER TABLE issue_workflow
    DROP COLUMN IF EXISTS run_budget_token_allowance,
    DROP COLUMN IF EXISTS run_budget_token_reserve;
