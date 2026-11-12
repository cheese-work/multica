-- CHE-2860 (A1c2b): the task a resumed refusal queued, set in the worker's
-- transaction together with state = 'run_queued'.
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS task_id UUID;
