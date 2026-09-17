-- name: GetIssueCheckpoint :one
-- Owner-scoped lookup: the checkpoint written by one agent's run is never
-- read as another agent's coverage, even on the same issue.
SELECT * FROM issue_checkpoint
WHERE issue_id = $1 AND agent_id = $2 AND workspace_id = $3;

-- name: UpsertIssueCheckpoint :one
-- Idempotent on (issue_id, agent_id) via uq_issue_checkpoint_owner (migration
-- 476): only the most recent coverage per issue+agent is ever a useful
-- checkpoint, so a later claim's write replaces the earlier one rather than
-- accumulating history rows.
INSERT INTO issue_checkpoint (
    workspace_id, issue_id, agent_id, issue_revision, candidate_id,
    coverage, accepted_decisions, obligations, blockers,
    next_permitted_action, evidence, resolved_threads, built_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (issue_id, agent_id) DO UPDATE SET
    issue_revision         = EXCLUDED.issue_revision,
    candidate_id           = EXCLUDED.candidate_id,
    coverage               = EXCLUDED.coverage,
    accepted_decisions     = EXCLUDED.accepted_decisions,
    obligations            = EXCLUDED.obligations,
    blockers               = EXCLUDED.blockers,
    next_permitted_action  = EXCLUDED.next_permitted_action,
    evidence               = EXCLUDED.evidence,
    resolved_threads       = EXCLUDED.resolved_threads,
    built_at               = EXCLUDED.built_at,
    updated_at             = now()
RETURNING *;
