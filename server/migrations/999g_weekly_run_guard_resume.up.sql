-- CHE-6172: a weekly run guard refusal keeps the refused trigger so one
-- approved card can run it again, and an approval grants a bounded allowance.
-- state: open (waiting for the card), approved (resume owed), resumed,
-- refused (the guard or another gate refused the resumed run), superseded.
CREATE TABLE IF NOT EXISTS weekly_run_guard_refusal (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    agent_id UUID,
    trigger TEXT NOT NULL DEFAULT '',
    actor_user_id UUID,
    trigger_comment_id UUID,
    source_wakeup_id UUID,
    source_task_id UUID,
    runs BIGINT NOT NULL,
    state TEXT NOT NULL DEFAULT 'open'
        CHECK (state IN ('open', 'approved', 'resumed', 'refused', 'superseded')),
    action_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One grant per approved card; it counts for 7 days after created_at.
CREATE TABLE IF NOT EXISTS weekly_run_guard_allowance (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    runs INTEGER NOT NULL CHECK (runs > 0),
    action_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
