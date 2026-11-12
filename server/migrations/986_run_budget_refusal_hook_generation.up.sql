-- CHE-2856: a window's refusal row is revived when a later refusal of the same
-- window finds its hook retired or superseded. Each revival bumps the
-- generation, so a step of an older advance cannot finish or retire the new hook.
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS hook_generation INTEGER NOT NULL DEFAULT 0;
