-- CHE-488: durable stage-completion-wake identity.
--
-- notifyParentOfChildDone / notifyParentsOfBatchChildDone (issue_child_done.go)
-- fire the parent's "Stage N is complete" system comment + assignee wake when
-- a stage barrier closes. Before this migration nothing recorded that a given
-- (parent, stage) completion had already woken its assignee: the only guards
-- were the transition check (prevTerminal || !nowTerminal) and
-- HasPendingTaskForIssueAndAgent, and neither survives a child being reopened
-- (terminal -> non-terminal) and re-closed later — a genuine fresh transition
-- each time, and by then the earlier task is long finished so the pending-task
-- dedup is a no-op. Two system comments were observed 15h44m apart for the
-- same parent/stage/child in production (CHE-488).
--
-- stage_generation tracks a per-(parent_issue_id, stage) counter that bumps
-- exactly when a staged child of that stage is reopened (terminal ->
-- non-terminal). generation 0 is the initial/never-reopened generation.
--
-- stage_completion_wake records that a wake was already emitted for a given
-- (parent_issue_id, stage, generation). Its UNIQUE index (migration 476) makes
-- "at most one wake per generation" a DB-enforced invariant instead of a
-- best-effort read-then-write check, so two concurrent closes of the same
-- stage race at the database and only one wins — the same pattern as
-- idx_one_live_rerun_per_source_task_actor (migration 474).
--
-- stage is NOT NULL: the unstaged sibling set is treated as a single implicit
-- stage (matching stageBarrierClosed) and is recorded here under stage 0,
-- which issue.stage's own CHECK (stage IS NULL OR stage >= 1) never produces,
-- so 0 cannot collide with a real stage value.
-- workspace_id is carried directly (not joined through parent_issue_id) so
-- workspace teardown (DeleteWorkspaceLeafData) can delete straight off the
-- (workspace_id) index, matching every other workspace-scoped leaf table —
-- see server/internal/handler/workspace_delete_manifest_test.go.
CREATE TABLE IF NOT EXISTS stage_generation (
    workspace_id UUID NOT NULL,
    parent_issue_id UUID NOT NULL,
    stage INTEGER NOT NULL,
    generation INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (parent_issue_id, stage)
);

CREATE TABLE IF NOT EXISTS stage_completion_wake (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    parent_issue_id UUID NOT NULL,
    stage INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    wake_comment_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
