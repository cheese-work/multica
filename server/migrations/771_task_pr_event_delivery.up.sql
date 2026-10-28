-- Delivery receipts of PR events to one exact run (CHE-2298 a2): the same
-- pending/delivering/delivered/failed states as task_supplement, keyed on the
-- issue_pr_event instead of a member comment. No foreign keys.
CREATE TABLE IF NOT EXISTS task_pr_event_delivery (
    task_id UUID NOT NULL,
    event_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'delivering', 'delivered', 'failed')),
    failure_reason TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ
);
