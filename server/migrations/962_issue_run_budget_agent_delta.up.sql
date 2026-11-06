-- Runs of credit an agent granted (CHE-3251). The metered-spend limit bounds
-- these runs; a run inside a person's allowance is never counted here.
ALTER TABLE issue_run_budget_entry ADD COLUMN IF NOT EXISTS agent_delta INTEGER NOT NULL DEFAULT 0;
