-- CHE-2297 L1a: one row per run-budget notice already written, so concurrent
-- refusals and retries write each notice at most once per budget window.
-- window_id names the budget window (its start time); kind is warn80 or
-- exhausted. The unique index lives in migration 754.
CREATE TABLE IF NOT EXISTS run_budget_notice (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    window_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    comment_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
