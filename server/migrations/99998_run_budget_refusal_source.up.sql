-- CHE-2860 (A1c2b slice 2): the wakeup rule or the failed run whose own
-- dispatch a budget refusal stopped. The resume runs that dispatch again and
-- binds the refusal to the run it delivers.
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS source_wakeup_id UUID;
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS source_task_id UUID;
