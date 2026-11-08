-- Admission deferral (CHE-4650): reviewer runs held back while the PR head is
-- not admissible. The record is the only state; it holds no slot.

-- name: ListAdmissionPullRequests :many
-- The open or draft PRs linked to the issue, with the head's merge verdict.
-- api_merge_state_status counts only when its snapshot is for the current head.
SELECT pr.id, pr.head_sha,
       CASE WHEN pr.snapshot_head_sha = pr.head_sha THEN COALESCE(pr.api_merge_state_status, '') ELSE '' END::text AS merge_state
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = sqlc.arg('issue_id') AND pr.state IN ('open', 'draft')
LIMIT 2;

-- name: ListAdmissionCheckRuns :many
-- The runs of the named checks on one head.
SELECT name, status, conclusion
FROM github_pull_request_check_run
WHERE pr_id = sqlc.arg('pr_id') AND head_sha = sqlc.arg('head_sha') AND name = ANY(sqlc.arg('names')::text[]);

-- name: UpsertAdmissionDeferral :exec
-- A repeat deferral on the same head keeps created_at and expires_at, so
-- repeated triggers never extend the 24 h bound; a new head starts afresh.
INSERT INTO admission_deferral (issue_id, agent_id, trigger, head_sha, reasons, actor_user_id, trigger_comment_id, taint_ids)
VALUES (sqlc.arg('issue_id'), sqlc.arg('agent_id'), sqlc.arg('trigger'), sqlc.arg('head_sha'), sqlc.arg('reasons')::text[],
        sqlc.narg('actor_user_id'), sqlc.narg('trigger_comment_id'), sqlc.arg('taint_ids')::text[])
ON CONFLICT (issue_id, agent_id) DO UPDATE SET
  trigger = EXCLUDED.trigger,
  reasons = EXCLUDED.reasons,
  head_sha = EXCLUDED.head_sha,
  actor_user_id = COALESCE(EXCLUDED.actor_user_id, admission_deferral.actor_user_id),
  trigger_comment_id = COALESCE(EXCLUDED.trigger_comment_id, admission_deferral.trigger_comment_id),
  taint_ids = (SELECT COALESCE(array_agg(DISTINCT t ORDER BY t), '{}')
                 FROM unnest(CASE WHEN admission_deferral.head_sha = EXCLUDED.head_sha THEN admission_deferral.taint_ids ELSE '{}' END || EXCLUDED.taint_ids) AS t),
  created_at = CASE WHEN admission_deferral.head_sha = EXCLUDED.head_sha THEN admission_deferral.created_at ELSE now() END,
  expires_at = CASE WHEN admission_deferral.head_sha = EXCLUDED.head_sha THEN admission_deferral.expires_at ELSE now() + interval '24 hours' END;
