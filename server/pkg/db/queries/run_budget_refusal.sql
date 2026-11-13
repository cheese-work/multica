-- name: GetOpenRunBudgetRefusal :one
-- The non-terminal refusal of one issue window (CHE-2798 A1c1a).
SELECT * FROM run_budget_refusal
WHERE issue_id = $1 AND window_id = $2
  AND state IN ('open', 'resume_pending', 'still_blocked');

-- name: GetRunBudgetRefusal :one
SELECT * FROM run_budget_refusal WHERE id = $1;

-- name: InsertRunBudgetRefusal :one
-- Tx R of the exhaustion hook (CHE-2798 A1c1b): a retry finds the row already
-- there and gets no row back.
INSERT INTO run_budget_refusal (
    id, workspace_id, issue_id, window_id, status, occurrence_activity_id, blocked, agent_id, trigger,
    state, state_reason, taint_ids, actor_user_id, trigger_comment_id, hook_phase)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, 'intent')
ON CONFLICT (issue_id, window_id) DO NOTHING
RETURNING *;

-- name: ReviveRunBudgetRefusal :one
-- A refusal of a window whose row no longer applies (its hook ran or was
-- retired, or a status move superseded it) is a new refusal: the row starts
-- again at intent for the new occurrence. The window's notice claims stay, so
-- the notice and the recovery run are not given twice.
UPDATE run_budget_refusal
SET status = $2, occurrence_activity_id = $3, blocked = $4, agent_id = $5, trigger = $6,
    state_reason = $7, state = $8, taint_ids = $9, actor_user_id = $10, trigger_comment_id = $11, task_id = NULL, hook_phase = 'intent', hook_reason = NULL,
    hook_generation = hook_generation + 1, updated_at = now()
WHERE id = $1
RETURNING *;

-- name: MergeRunBudgetRefusalTaint :exec
-- A later requester of the same window joins the hook that answers it: the
-- recovery run carries the taint of every request the hook answers. A subset
-- writes nothing.
UPDATE run_budget_refusal
SET taint_ids = ARRAY(SELECT DISTINCT t FROM unnest(taint_ids || @taint::text[]) AS t ORDER BY t)
WHERE id = @id::uuid AND NOT taint_ids @> @taint::text[];

-- name: GetRunBudgetRefusalByWindow :one
SELECT * FROM run_budget_refusal WHERE issue_id = $1 AND window_id = $2;

-- name: GetRunBudgetRefusalByAction :one
-- The refusal a budget approval linked (CHE-2860): the post-commit notify finds it by action.
SELECT * FROM run_budget_refusal WHERE action_id = $1;

-- name: LockRunBudgetRefusal :one
SELECT * FROM run_budget_refusal WHERE id = $1 FOR UPDATE;

-- name: NewestIssueStatusChange :one
-- The status occurrence the issue is in: its newest status_changed row, with the
-- cause kind and the refusal a run-budget block named.
SELECT id,
       COALESCE(details->'trigger'->'cause'->>'kind', '')::text AS cause_kind,
       COALESCE(details->'trigger'->'cause'->'ref'->>'refusal_id', '')::text AS refusal_id
FROM activity_log
WHERE issue_id = $1 AND action = 'status_changed'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: SetRunBudgetRefusalHook :exec
-- Advances the hook. A NULL occurrence keeps the stored one.
UPDATE run_budget_refusal
SET hook_phase = @hook_phase::text,
    hook_reason = sqlc.narg('hook_reason')::text,
    blocked = COALESCE(sqlc.narg('blocked')::boolean, blocked),
    occurrence_activity_id = COALESCE(sqlc.narg('occurrence_activity_id')::uuid, occurrence_activity_id),
    updated_at = now()
WHERE id = @id::uuid;

-- name: SupersedeRunBudgetRefusalsExcept :exec
-- A refusal for a later window ends every earlier non-terminal refusal of the
-- issue and its unfinished hook (P2r supersession a).
UPDATE run_budget_refusal
SET state = 'superseded', state_reason = 'later_window',
    hook_phase = CASE WHEN hook_phase IN ('intent', 'blocked') THEN 'retired' ELSE hook_phase END,
    hook_reason = CASE WHEN hook_phase IN ('intent', 'blocked') THEN 'later_window' ELSE hook_reason END,
    updated_at = now()
WHERE issue_id = $1 AND id <> $2 AND state IN ('open', 'resume_pending', 'still_blocked');

-- name: SupersedeRunBudgetRefusalsOnStatusChange :exec
-- A status move starts its own work: every non-terminal refusal of the issue
-- ends, and so does its unfinished hook (P2r supersession b).
UPDATE run_budget_refusal
SET state = 'superseded', state_reason = 'status_moved',
    hook_phase = CASE WHEN hook_phase IN ('intent', 'blocked') THEN 'retired' ELSE hook_phase END,
    hook_reason = CASE WHEN hook_phase IN ('intent', 'blocked') THEN 'status_moved' ELSE hook_reason END,
    updated_at = now()
WHERE issue_id = $1 AND state IN ('open', 'resume_pending', 'still_blocked');

-- name: ListStaleRunBudgetHooks :many
-- Hooks a crashed callback left unfinished: the sweep advances them.
SELECT id FROM run_budget_refusal
WHERE hook_phase IN ('intent', 'blocked') AND updated_at < @before::timestamptz
ORDER BY updated_at
LIMIT 50;

-- name: RetireRunBudgetRefusalHook :exec
-- Ends an unfinished hook of one generation with a reason: a revived hook is
-- not retired by a step of an older one. The refusal's own state is left to the
-- supersession writers.
UPDATE run_budget_refusal
SET hook_phase = 'retired', hook_reason = @reason::text, updated_at = now()
WHERE id = @id::uuid AND hook_generation = @generation::int AND hook_phase IN ('intent', 'blocked');

-- name: MarkRunBudgetRefusalBlocked :exec
-- Tx B's phase advance, written in the block's own transaction: the refusal's
-- block is the status occurrence that move recorded.
UPDATE run_budget_refusal
SET hook_phase = 'blocked', blocked = true, occurrence_activity_id = @occurrence_activity_id::uuid, updated_at = now()
WHERE id = @id::uuid AND hook_phase = 'intent';

-- name: LinkRunBudgetRefusal :one
-- Link (CHE-2798 P2r, CHE-2860): a budget approval claims the refusal it answers.
-- An unfinished intent hook is retired in the same statement, so the block is
-- cancelled and the worker admits the run directly. A refusal still waiting for
-- its worker is linked again: the latest approval's action and window win, so a
-- top-up that commits while the worker is rolling back is not lost. No row back:
-- the refusal is superseded or already resumed.
UPDATE run_budget_refusal
SET state = 'resume_pending', state_reason = NULL, action_id = @action_id::uuid, resume_window_id = @resume_window_id::text,
    hook_phase = CASE WHEN hook_phase = 'intent' THEN 'retired' ELSE hook_phase END,
    hook_reason = CASE WHEN hook_phase = 'intent' THEN 'resume_pending' ELSE hook_reason END,
    updated_at = now()
WHERE id = @id::uuid AND issue_id = @issue_id::uuid AND state IN ('open', 'still_blocked', 'resume_pending')
RETURNING *;

-- name: ListResumePendingRunBudgetRefusals :many
-- Linked refusals a crashed or missed notify left unresumed: the sweep resumes them.
SELECT id FROM run_budget_refusal
WHERE state = 'resume_pending' AND updated_at < @before::timestamptz
ORDER BY updated_at
LIMIT 50;

-- name: FinishRunBudgetRefusalResume :exec
-- The worker's terminal write, in its own transaction: run_queued (with the task)
-- or status_restored.
UPDATE run_budget_refusal
SET state = @state::text, state_reason = NULL, task_id = sqlc.narg('task_id')::uuid, updated_at = now()
WHERE id = @id::uuid AND state = 'resume_pending';

-- name: FailRunBudgetRefusalResume :exec
-- The second transaction after a rolled-back resume: still_blocked (retryable) or
-- superseded, with the reason. Only a row still waiting for the worker, and still
-- linked to the action the worker resumed, changes: a newer approval's link stays.
UPDATE run_budget_refusal
SET state = @state::text, state_reason = @reason::text,
    hook_phase = CASE WHEN @state::text = 'superseded' AND hook_phase IN ('intent', 'blocked') THEN 'retired' ELSE hook_phase END,
    hook_reason = CASE WHEN @state::text = 'superseded' AND hook_phase IN ('intent', 'blocked') THEN @reason::text ELSE hook_reason END,
    updated_at = now()
WHERE id = @id::uuid AND state = 'resume_pending' AND action_id = @action_id::uuid;

-- name: UnionTaskTaint :exec
-- The run a refusal ended on carries the refusal's taint (CHE-2860). Monotonic:
-- the task keeps what it has (the agent_task_inherit_taint trigger unions it
-- again), so a join never makes a run cleaner.
UPDATE agent_task_queue
SET taint_ids = ARRAY(SELECT DISTINCT t FROM unnest(taint_ids || @taint::text[]) AS t ORDER BY t)
WHERE id = @id::uuid AND NOT taint_ids @> @taint::text[];

-- name: SetRunBudgetRefusalSource :exec
-- The native source a refusal stopped (CHE-2860): a derived wakeup rule or a
-- failed run. Written with the insert or the revive, NULLs included.
UPDATE run_budget_refusal
SET source_wakeup_id = sqlc.narg('source_wakeup_id')::uuid, source_task_id = sqlc.narg('source_task_id')::uuid
WHERE id = @id::uuid;

-- name: BindRunBudgetRefusalToRun :many
-- A native dispatch (wakeup tick, system wakeup, retry) delivered the run the
-- refusal stopped, in the dispatch's own transaction: the refusal ends
-- run_queued on that exact run. Only a refusal the approval already linked is
-- bound; the taint it held is returned for the run.
UPDATE run_budget_refusal
SET state = 'run_queued', task_id = @task_id::uuid, state_reason = NULL, updated_at = now()
WHERE issue_id = @issue_id::uuid AND state = 'resume_pending' AND blocked = false
  AND ((sqlc.narg('source_wakeup_id')::uuid IS NOT NULL AND source_wakeup_id = sqlc.narg('source_wakeup_id')::uuid)
    OR (sqlc.narg('source_task_id')::uuid IS NOT NULL AND source_task_id = sqlc.narg('source_task_id')::uuid))
RETURNING taint_ids;

