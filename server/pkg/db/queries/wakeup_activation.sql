-- name: GetWakeupDefinitionActivation :one
SELECT * FROM wakeup_definition_activation WHERE name= @name;

-- name: EnsureWakeupDefinitionActivation :exec
INSERT INTO wakeup_definition_activation(name) VALUES(@name) ON CONFLICT (name) DO NOTHING;

-- name: LockWakeupDefinitionActivation :one
-- Serializes concurrent runners: a page, its cursor and its counters commit
-- together under this lock.
SELECT * FROM wakeup_definition_activation WHERE name= @name FOR UPDATE;

-- name: AdvanceWakeupDefinitionActivation :exec
UPDATE wakeup_definition_activation
SET state= @state, cursor= @cursor, scanned= @scanned, inserted= @inserted, last_error= sqlc.narg(last_error)::text,
 updated_at= clock_timestamp(), completed_at= CASE WHEN @state::text='complete' THEN clock_timestamp() ELSE NULL END
WHERE name= @name;

-- name: CountWakeupDefinitionsBeyondBackfill :one
-- Verification: before activation the only definitions are backfilled issue
-- overrides of platform rules. A custom root, a project or workspace record, or
-- a definition that selects events would mean work could be queued from it.
SELECT count(*) FROM issue_wakeup_definition
WHERE (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id= sqlc.narg(workspace_id)::uuid)
 AND (scope_kind<>'issue' OR root OR cardinality(event_types)>0);

-- name: NoteWakeupDefinitionActivationError :exec
UPDATE wakeup_definition_activation SET last_error= sqlc.narg(last_error)::text, updated_at= clock_timestamp() WHERE name= @name;
