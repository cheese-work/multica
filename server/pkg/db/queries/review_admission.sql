-- CHE-4650: reviewer-run admission. A reviewer run starts only on an
-- admissible PR head; otherwise the deferral is recorded for 24 h.

-- name: GetIssueAdmissionPR :one
-- The linked GitHub PR a reviewer run would review: the newest open or draft
-- one. No row when the issue has none, so there is nothing to judge.
SELECT pr.*
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
WHERE ipr.issue_id = $1 AND pr.state IN ('open', 'draft') AND pr.head_sha <> ''
ORDER BY pr.pr_updated_at DESC
LIMIT 1;

-- name: ListAdmissionCheckRuns :many
-- The stored checks of one PR head. Rows of an older head are not evidence.
SELECT name, status, conclusion FROM github_pull_request_check_run
WHERE pr_id = $1 AND head_sha = $2;

-- name: MarkReviewAdmissionDeferred :exec
-- A repeat keeps the latest non-null actor and comment and only adds taint, as
-- MarkStageWaiting does.
INSERT INTO review_admission_deferral (issue_id, agent_id, trigger, head_sha, reasons, created_at, expires_at, actor_user_id, trigger_comment_id, taint_ids)
VALUES (sqlc.arg('issue_id')::uuid, sqlc.arg('agent_id')::uuid, sqlc.arg('trigger')::text,
        sqlc.arg('head_sha')::text, sqlc.arg('reasons')::text[], now(), now() + interval '24 hours',
        sqlc.narg('actor_user_id')::uuid, sqlc.narg('trigger_comment_id')::uuid, sqlc.arg('taint_ids')::text[])
-- A repeat on the same head keeps the first deadline, so repeated triggers
-- cannot postpone recovery; a new head starts a new window.
ON CONFLICT (issue_id, agent_id) DO UPDATE SET
  trigger = EXCLUDED.trigger, reasons = EXCLUDED.reasons,
  created_at = CASE WHEN review_admission_deferral.head_sha = EXCLUDED.head_sha
                    THEN review_admission_deferral.created_at ELSE EXCLUDED.created_at END,
  expires_at = CASE WHEN review_admission_deferral.head_sha = EXCLUDED.head_sha
                    THEN review_admission_deferral.expires_at ELSE EXCLUDED.expires_at END,
  head_sha = EXCLUDED.head_sha,
  actor_user_id = COALESCE(EXCLUDED.actor_user_id, review_admission_deferral.actor_user_id),
  trigger_comment_id = COALESCE(EXCLUDED.trigger_comment_id, review_admission_deferral.trigger_comment_id),
  taint_ids = (SELECT COALESCE(array_agg(DISTINCT t ORDER BY t), '{}')
                 FROM unnest(review_admission_deferral.taint_ids || EXCLUDED.taint_ids) AS t);

-- name: ListReviewAdmissionDeferrals :many
-- The records the release evaluates: all of them (fallback tick), or those of
-- the given issues (a GitHub event for their PR).
SELECT issue_id, agent_id, trigger, head_sha, expires_at, actor_user_id, trigger_comment_id, taint_ids
FROM review_admission_deferral
WHERE sqlc.narg('issue_ids')::uuid[] IS NULL OR issue_id = ANY(sqlc.narg('issue_ids')::uuid[])
ORDER BY created_at;

-- name: GetReviewAdmissionDeferralExpiry :one
-- When the stored deferral of an issue, agent and PR head runs out. No row when
-- there is none for that head: a deadline never carries over to another head.
SELECT expires_at FROM review_admission_deferral
WHERE issue_id = sqlc.arg('issue_id')::uuid AND agent_id = sqlc.arg('agent_id')::uuid
  AND head_sha = sqlc.arg('head_sha')::text;

-- name: DeleteReviewAdmissionDeferral :exec
-- Deletes the exact record the caller read: a replacement the guard wrote for a
-- new head meanwhile has another head or deadline and stays.
DELETE FROM review_admission_deferral
WHERE issue_id = sqlc.arg('issue_id')::uuid AND agent_id = sqlc.arg('agent_id')::uuid
  AND head_sha = sqlc.arg('head_sha')::text AND expires_at = sqlc.arg('expires_at')::timestamptz;
