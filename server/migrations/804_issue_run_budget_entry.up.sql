-- CHE-2297 L2a: run-budget ledger. One row per credit or debit of an issue's
-- run budget; balance = sum of the deltas. No foreign keys and no deletes:
-- usage history is kept. The unique index is migration 805, the workspace
-- index migration 806. cap_delta is the part of runs_delta that moves the
-- cap (window credit, raise, cap edit); its sum is the cap the ledger holds.
-- tokens_delta is unused until token accounting (L2b).
CREATE TABLE IF NOT EXISTS issue_run_budget_entry (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    window_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    event_key TEXT NOT NULL,
    runs_delta INTEGER NOT NULL DEFAULT 0,
    cap_delta INTEGER NOT NULL DEFAULT 0,
    tokens_delta BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
