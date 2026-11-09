-- CHE-2860: the comment and the person behind a refused run, so the resumed run
-- is attributed to them and reads the comment that asked for it.
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS actor_user_id UUID;
ALTER TABLE run_budget_refusal ADD COLUMN IF NOT EXISTS trigger_comment_id UUID;
