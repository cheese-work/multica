-- name: GetOpenRunBudgetRefusal :one
-- The non-terminal refusal of one issue window (CHE-2798 A1c1a).
SELECT * FROM run_budget_refusal
WHERE issue_id = $1 AND window_id = $2
  AND state IN ('open', 'resume_pending', 'still_blocked');

-- name: GetRunBudgetRefusal :one
SELECT * FROM run_budget_refusal WHERE id = $1;
