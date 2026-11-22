-- Step watchdog ledger (CHE-6116 S2). One row per action taken on one status
-- occurrence of an issue. occurrence_key is the activity_log id of the newest
-- status_changed row, or created:<issue id> when the issue has no status
-- history, so it is never null and the unique index (next migration) is
-- null-safe. No FKs: issue and workspace delete sweep this table in app code.
CREATE TABLE issue_step_watchdog_action (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    occurrence_key TEXT NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('would_act', 'block_budget', 'redispatch', 'notify_only', 'notify')),
    outcome TEXT NOT NULL DEFAULT '',
    details JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
