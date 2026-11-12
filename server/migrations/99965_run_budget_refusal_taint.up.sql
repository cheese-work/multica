-- CHE-2856: the taint of the task whose request the budget refused. A restart
-- sweep rebuilds the hook's event from this row, so the recovery run it creates
-- must read the taint from here: a run made without it could be elevated by an
-- active grant.
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS taint_ids TEXT[] NOT NULL DEFAULT '{}';
