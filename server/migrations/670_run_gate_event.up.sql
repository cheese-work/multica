-- CHE-1918 (CHE-1917 Slice 0a): shadow ledger of the events that start leader
-- runs. One row per (source, issue, event, candidate): the comment that fell
-- back to a squad leader, or the pr_merged / pr_checks_failed wakeup input.
-- Capture only. Nothing reads this table to admit, fold or skip a run, and a
-- failed write never stops the event it describes.
--
-- No foreign keys, like governed_vote_record: the row is measurement evidence
-- for Slice 0b and must survive the run, receipt and wakeup it describes
-- (issue_wakeup_receipt is expired 7 days after processing).
-- Comment text is never stored; trigger_comment_id points at it.
CREATE TABLE run_gate_event (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('comment_leader', 'wakeup_pr_merged', 'wakeup_pr_checks_failed')),
    -- comment: the comment id. wakeup: the issue_wakeup_pr_event key
    -- (github:owner/repo#N, plus :head sha for a failed check).
    event_key TEXT NOT NULL,
    -- comment: the leader agent id. wakeup: the wakeup id.
    candidate_key TEXT NOT NULL,
    -- comment: the trigger source (issue_assignee, mention_squad_leader,
    -- thread_parent, conversation_continuation). wakeup: 'system_wakeup'.
    origin TEXT NOT NULL,
    author_type TEXT,
    non_leader_agent_reply BOOLEAN NOT NULL DEFAULT false,
    trigger_comment_id UUID,
    system_rule TEXT,
    wakeup_id UUID,
    wakeup_revision BIGINT,
    receipt_id UUID,
    -- The agent a run would execute as (comment: the leader; wakeup: the
    -- resolved target, NULL for a member or an unresolved target).
    agent_id UUID,
    squad_id UUID,
    issue_revision BIGINT NOT NULL,
    issue_status TEXT NOT NULL,
    pr_head_sha TEXT,
    -- comment: comment.created_at. wakeup: when the input was recorded.
    event_time TIMESTAMPTZ NOT NULL,
    -- Would today's native fold take this input into an already queued run?
    -- Read once at capture; it says nothing about what happened afterwards.
    native_fold_eligible BOOLEAN NOT NULL,
    native_fold_task_id UUID,
    -- Live tasks of agent_id on the issue at capture, newest first, at most 20.
    queue_snapshot JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The dispatched / running / waiting task at capture, if any.
    active_task_id UUID,
    -- The disposition the producer itself returned, copied verbatim
    -- (queued, coalesced, deferred, blocked ...). NULL until it returned and
    -- always NULL for wakeups; join to the run for those.
    admission_status TEXT,
    admission_reason TEXT,
    admission_at TIMESTAMPTZ,
    seen_count INTEGER NOT NULL DEFAULT 1,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, issue_id, event_key, candidate_key)
);

CREATE INDEX run_gate_event_window_idx ON run_gate_event (workspace_id, source, event_time);
CREATE INDEX run_gate_event_comment_idx ON run_gate_event (trigger_comment_id) WHERE trigger_comment_id IS NOT NULL;
CREATE INDEX run_gate_event_wakeup_idx ON run_gate_event (wakeup_id) WHERE wakeup_id IS NOT NULL;
