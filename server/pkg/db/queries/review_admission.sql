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
INSERT INTO review_admission_deferral (issue_id, agent_id, trigger, head_sha, reasons, created_at, expires_at)
VALUES (sqlc.arg('issue_id')::uuid, sqlc.arg('agent_id')::uuid, sqlc.arg('trigger')::text,
        sqlc.arg('head_sha')::text, sqlc.arg('reasons')::text[], now(), now() + interval '24 hours')
-- A repeat on the same head keeps the first deadline, so repeated triggers
-- cannot postpone recovery; a new head starts a new window.
ON CONFLICT (issue_id, agent_id) DO UPDATE SET
  trigger = EXCLUDED.trigger, reasons = EXCLUDED.reasons,
  created_at = CASE WHEN review_admission_deferral.head_sha = EXCLUDED.head_sha
                    THEN review_admission_deferral.created_at ELSE EXCLUDED.created_at END,
  expires_at = CASE WHEN review_admission_deferral.head_sha = EXCLUDED.head_sha
                    THEN review_admission_deferral.expires_at ELSE EXCLUDED.expires_at END,
  head_sha = EXCLUDED.head_sha;
