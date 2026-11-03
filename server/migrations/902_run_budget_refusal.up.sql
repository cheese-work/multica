-- CHE-2798 A1c1a: one row per refused run, so a budget approval can resume
-- the work that was refused. state tracks the resume, hook_phase the
-- exhaustion hook; state_reason carries the suffix of a still_blocked or
-- superseded state. status is NULL when the issue was blocked before this
-- table existed. The unique index lives in migration 903.
CREATE TABLE IF NOT EXISTS run_budget_refusal (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    window_id TEXT NOT NULL,
    status TEXT,
    occurrence_activity_id UUID,
    blocked BOOLEAN NOT NULL DEFAULT false,
    agent_id UUID,
    trigger TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'open'
        CHECK (state IN ('open', 'resume_pending', 'still_blocked', 'run_queued', 'status_restored', 'superseded')),
    state_reason TEXT,
    hook_phase TEXT NOT NULL DEFAULT 'intent'
        CHECK (hook_phase IN ('intent', 'blocked', 'done', 'retired')),
    hook_reason TEXT,
    action_id UUID,
    resume_window_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
