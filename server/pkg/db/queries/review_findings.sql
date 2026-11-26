-- name: ListPriorChangesFindingsForPR :many
-- Findings JSON of the earlier CHANGES verdicts of one PR lineage since the issue last passed review or hit the round cap (CHE-9307). Same window as CountChangesRoundsForPR.
SELECT COALESCE(a.details->>'findings', '[]')::text AS findings FROM activity_log a
WHERE a.issue_id = $1 AND a.action = 'review_changes_requested'
  AND COALESCE(a.details->>'pull_request_id', '') = sqlc.arg('pull_request_id')::text
  AND a.created_at > COALESCE((
    SELECT max(m.created_at) FROM activity_log m
    WHERE m.issue_id = $1 AND m.action IN ('review_passed', 'review_rounds_capped')
  ), '-infinity'::timestamptz)
ORDER BY a.created_at;

-- name: ListReviewVerdictFindings :many
-- Recorded review verdicts of a workspace since a time, with their findings JSON (CHE-9307). The category counts and the finding list read this.
SELECT i.number AS issue_number, a.action, a.created_at, COALESCE(a.details->>'findings', '[]')::text AS findings
FROM activity_log a JOIN issue i ON i.id = a.issue_id
WHERE a.workspace_id = $1 AND a.action IN ('review_changes_requested', 'review_passed')
  AND a.created_at >= $2
ORDER BY a.created_at DESC;
