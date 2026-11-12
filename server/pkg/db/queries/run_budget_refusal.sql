-- name: GetOpenRunBudgetRefusal :one
-- The non-terminal refusal of one issue window (CHE-2798 A1c1a).
SELECT * FROM run_budget_refusal
WHERE issue_id = $1 AND window_id = $2
  AND state IN ('open', 'resume_pending', 'still_blocked');

-- name: GetRunBudgetRefusal :one
SELECT * FROM run_budget_refusal WHERE id = $1;

-- name: TaintBudgetRecoveryRuns :exec
-- A requester that joined a hook after its notice committed: the recovery run
-- already exists, so it takes the requester's taint, whether it is still queued
-- waiting for a local directory, or already started (the task trigger unions it with what the run has, and a
-- started run reads its taint on every call).
UPDATE agent_task_queue SET taint_ids = @taint::text[]
WHERE issue_id = @issue_id::uuid AND task_kind = 'budget_recovery'
  AND status IN ('queued', 'dispatched', 'waiting_local_directory', 'running');
