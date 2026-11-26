-- name: ListDesignAuthorTasks :many
-- Design guard (CHE-7064 D1.5): the task rows behind a doop author ledger, so a
-- lab comes from Multica's own record and never from doop's copy. The runtime
-- provider is a LEFT JOIN: a task whose runtime row is gone still returns, with
-- an empty provider, and its lab is then unknown. Ids Multica does not know are
-- absent from the result.
SELECT t.id, t.agent_id, t.issue_id, COALESCE(r.provider, '')::text AS provider, COALESCE(t.claimed_model, '')::text AS claimed_model
FROM agent_task_queue t
LEFT JOIN agent_runtime r ON r.id = t.runtime_id
WHERE t.id = ANY(@ids::uuid[]);

-- name: GetNewestDesignVerdict :one
-- Design guard (CHE-7064 D1.4, C6): the newest design_review_verdict of one
-- issue for one doop attestation. Reads idx_activity_log_issue_keyset (issue_id,
-- created_at DESC, id DESC); no row means no verdict.
SELECT id, actor_id, created_at, details
FROM activity_log
WHERE issue_id = @issue_id
  AND action = 'design_review_verdict'
  AND details->>'attestation_id' = @attestation_id::text
ORDER BY created_at DESC, id DESC
LIMIT 1;
