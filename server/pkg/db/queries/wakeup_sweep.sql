-- name: ClaimWakeupDefinitionSweep :one
-- Leases one definition whose activation sweep is pending, found by the partial
-- index on exactly that condition. SKIP LOCKED lets concurrent schedulers take
-- different definitions; the lease (sweep_retry_at pushed forward) outlives the
-- claiming statement, so a pass can work in many short transactions and a
-- crashed one is retried when the lease lapses. A revision the sweep has not
-- seen discards the old cursor: a new rule acts on facts already true only when
-- its root is revision 1, every edit baselines them.
WITH pick AS (
 SELECT workspace_id,scope_kind,scope_id,rule_key FROM issue_wakeup_definition
 WHERE (NOT sweep_done OR sweep_revision <> revision) AND (sweep_retry_at IS NULL OR sweep_retry_at <= @now::timestamptz)
  AND (sqlc.narg(workspace_ids)::uuid[] IS NULL OR workspace_id = ANY(sqlc.narg(workspace_ids)::uuid[]))
 ORDER BY updated_at,rule_key LIMIT 1
 FOR UPDATE SKIP LOCKED
)
UPDATE issue_wakeup_definition d SET sweep_retry_at= @lease_until::timestamptz,sweep_done=false,
 sweep_cursor=CASE WHEN d.sweep_revision<>d.revision THEN NULL ELSE d.sweep_cursor END,
 sweep_baseline=CASE WHEN d.sweep_revision<>d.revision THEN (NOT d.root OR d.revision>1) ELSE d.sweep_baseline END,
 sweep_attempts=CASE WHEN d.sweep_revision<>d.revision THEN 0 ELSE d.sweep_attempts END,
 sweep_error=CASE WHEN d.sweep_revision<>d.revision THEN NULL ELSE d.sweep_error END,
 sweep_revision=d.revision
FROM pick WHERE d.workspace_id=pick.workspace_id AND d.scope_kind=pick.scope_kind AND d.scope_id=pick.scope_id AND d.rule_key=pick.rule_key
RETURNING d.*;

-- name: AdvanceWakeupDefinitionSweep :execrows
-- Moves the cursor past a batch and ends the lease; done when the scope has no
-- more issues. A definition revised since the claim is not touched: its sweep is
-- pending again under the new revision and the stale cursor is never written back.
UPDATE issue_wakeup_definition SET sweep_cursor= sqlc.narg(cursor)::uuid,sweep_done= @done::bool,sweep_retry_at=NULL,sweep_attempts=0,sweep_error=NULL
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id AND rule_key= @rule_key
 AND revision= @revision AND sweep_revision= @revision;

-- name: FailWakeupDefinitionSweep :execrows
-- A pass that hit a real database error is retried after a delay; once it has
-- failed max_attempts times in a row it is parked with the error visible, done
-- for this revision, so a broken definition cannot be retried forever.
UPDATE issue_wakeup_definition SET sweep_attempts=sweep_attempts+1,sweep_error= @error::text,
 sweep_retry_at=CASE WHEN sweep_attempts+1 >= @max_attempts::int THEN NULL ELSE @retry_at::timestamptz END,
 sweep_done=(sweep_attempts+1 >= @max_attempts::int)
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id AND rule_key= @rule_key
 AND revision= @revision AND sweep_revision= @revision;

-- name: ListSweepWorkspaceIssues :many
-- The next batch of a workspace-wide sweep, in keyset order on (workspace_id, id).
SELECT id FROM issue WHERE workspace_id= @workspace_id AND id> @after::uuid ORDER BY id LIMIT @batch_size;

-- name: ListSweepProjectIssues :many
-- The next batch of a project's sweep, in keyset order on (project_id, id).
SELECT id FROM issue WHERE project_id= @project_id AND workspace_id= @workspace_id AND id> @after::uuid ORDER BY id LIMIT @batch_size;

-- name: ListCapacityHeldWakeups :many
-- Default-derived instances a full pool left unapplied, oldest attempt first.
-- A refused retry touches updated_at, so every held instance gets its turn.
SELECT id,issue_id,workspace_id FROM issue_wakeup
WHERE capacity_reason IS NOT NULL AND NOT enabled AND default_rule_key IS NOT NULL AND system_rule IS NULL
 AND (sqlc.narg(workspace_ids)::uuid[] IS NULL OR workspace_id = ANY(sqlc.narg(workspace_ids)::uuid[]))
ORDER BY updated_at,id LIMIT @batch_size;

-- name: ClearEndedWakeupCapacityReason :exec
-- An instance a person, a pause, a timeout or its closing issue ended no longer
-- waits for capacity: dropping the reason takes it out of the retry list for good.
UPDATE issue_wakeup SET capacity_reason=NULL
WHERE id= @id AND capacity_reason IS NOT NULL AND NOT enabled AND (disabled_at IS NOT NULL OR paused_reason IS NOT NULL OR timed_out_at IS NOT NULL);
