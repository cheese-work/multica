-- CHE-2297 L2e: token accounting is active per workflow only when it stores
-- both values: the allowance A credited per budget window and the reservation
-- E held per admitted run (weighted tokens). NULL: token accounting is off.
ALTER TABLE issue_workflow
    ADD COLUMN IF NOT EXISTS run_budget_token_allowance bigint CHECK (run_budget_token_allowance > 0),
    ADD COLUMN IF NOT EXISTS run_budget_token_reserve bigint CHECK (run_budget_token_reserve > 0);
