-- name: GetIssueCheckpoint :one
-- Owner-scoped lookup: the checkpoint written by one agent's run is never
-- read as another agent's coverage, even on the same issue.
SELECT * FROM issue_checkpoint
WHERE issue_id = $1 AND agent_id = $2 AND workspace_id = $3;

-- name: UpsertIssueCheckpoint :one
-- Idempotent on (issue_id, agent_id) via uq_issue_checkpoint_owner (migration
-- 503): only the most recent coverage per issue+agent is ever a useful
-- checkpoint, so a later claim's write replaces the earlier one rather than
-- accumulating history rows.
--
-- The WHERE clause on the DO UPDATE is a monotonicity guard: of two
-- concurrent claims, the one with the OLDER built_at must not clobber a
-- newer write that landed first (e.g. a resolution recorded between this
-- claim's read and its upsert). A no-op UPDATE (WHERE false) still returns
-- the existing row via RETURNING, so callers see accurate stored state
-- either way.
--
-- workspace_id is included in the SET list so a corrected issue.workspace_id
-- is reflected here too — otherwise the row would silently retain a stale
-- value the read path (scoped on workspace_id) could no longer find.
INSERT INTO issue_checkpoint (
    workspace_id, issue_id, agent_id, issue_revision, candidate_id,
    coverage, accepted_decisions, obligations, blockers,
    next_permitted_action, evidence, resolved_threads, built_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (issue_id, agent_id) DO UPDATE SET
    workspace_id           = EXCLUDED.workspace_id,
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
WHERE issue_checkpoint.built_at <= EXCLUDED.built_at
RETURNING *;
