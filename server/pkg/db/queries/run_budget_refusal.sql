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
    state, state_reason, taint_ids, hook_phase)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, 'intent')
ON CONFLICT (issue_id, window_id) DO NOTHING
RETURNING *;

-- name: ReviveRunBudgetRefusal :one
-- A refusal of a window whose row no longer applies (its hook ran or was
-- retired, or a status move superseded it) is a new refusal: the row starts
-- again at intent for the new occurrence. The window's notice claims stay, so
-- the notice and the recovery run are not given twice.
UPDATE run_budget_refusal
SET status = $2, occurrence_activity_id = $3, blocked = $4, agent_id = $5, trigger = $6,
    state_reason = $7, state = $8, taint_ids = $9, hook_phase = 'intent', hook_reason = NULL,
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
