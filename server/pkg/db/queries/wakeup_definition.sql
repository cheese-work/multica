-- name: ListWakeupDefinitionsForRule :many
-- One rule's stored definitions that can apply to an issue: its workspace's,
-- its current project's (project_id may be NULL) and its own. Three exact
-- lookups on the full identity index, so cost and result stay bounded no
-- matter how many other definitions the workspace has.
SELECT d.workspace_id,d.scope_kind,d.scope_id,d.rule_key,d.root,d.config,d.revision,d.created_by,d.updated_by,d.created_at,d.updated_at FROM issue_wakeup_definition d
WHERE d.workspace_id= @workspace_id AND d.scope_kind='workspace' AND d.scope_id= @workspace_id AND d.rule_key= @rule_key
UNION ALL
SELECT d.workspace_id,d.scope_kind,d.scope_id,d.rule_key,d.root,d.config,d.revision,d.created_by,d.updated_by,d.created_at,d.updated_at FROM issue_wakeup_definition d
WHERE d.workspace_id= @workspace_id AND d.scope_kind='project' AND d.scope_id= sqlc.narg(project_id)::uuid AND d.rule_key= @rule_key
UNION ALL
SELECT d.workspace_id,d.scope_kind,d.scope_id,d.rule_key,d.root,d.config,d.revision,d.created_by,d.updated_by,d.created_at,d.updated_at FROM issue_wakeup_definition d
WHERE d.workspace_id= @workspace_id AND d.scope_kind='issue' AND d.scope_id= @issue_id AND d.rule_key= @rule_key;

-- name: InsertWakeupDefinitionIfAbsent :execrows
-- Backfill write: never replaces a definition that already exists.
INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config)
VALUES(@workspace_id,@scope_kind,@scope_id,@rule_key,false,@config)
ON CONFLICT (workspace_id,scope_kind,scope_id,rule_key) DO NOTHING;

-- name: ListCustomizedSystemWakeupsAfter :many
-- Backfill page: platform rules a person changed on their issue, in id order,
-- optionally of one workspace.
SELECT * FROM issue_wakeup
WHERE system_rule IS NOT NULL AND customized_at IS NOT NULL AND id> @after_id
 AND (sqlc.narg(workspace_id)::uuid IS NULL OR workspace_id= sqlc.narg(workspace_id)::uuid)
ORDER BY id LIMIT @page_limit;

-- name: LockWakeupDefinitionScope :exec
-- Serializes definition writes of one scope (revision checks, the per-scope
-- ceiling) for the rest of the transaction.
SELECT pg_advisory_xact_lock(hashtextextended(@scope_key::text, 0));

-- name: GetWakeupDefinition :one
SELECT * FROM issue_wakeup_definition
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id AND rule_key= @rule_key;

-- name: ListWakeupDefinitionsInScope :many
-- One scope's definitions; the per-scope ceiling keeps this small.
SELECT * FROM issue_wakeup_definition
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id
ORDER BY created_at,rule_key LIMIT 100;

-- name: CountWakeupDefinitionsInScope :one
SELECT count(*) FROM issue_wakeup_definition
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id;

-- name: InsertWakeupDefinition :one
-- Never replaces: a definition that already exists yields no row.
INSERT INTO issue_wakeup_definition(workspace_id,scope_kind,scope_id,rule_key,root,config,created_by,updated_by)
VALUES(@workspace_id,@scope_kind,@scope_id,@rule_key,@root,@config,@actor,@actor)
ON CONFLICT (workspace_id,scope_kind,scope_id,rule_key) DO NOTHING
RETURNING *;

-- name: UpdateWakeupDefinition :one
-- Compare-and-swap on the revision the caller observed; no row means it moved.
UPDATE issue_wakeup_definition
SET config= @config,revision=revision+1,updated_by= @actor,updated_at=clock_timestamp()
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id AND rule_key= @rule_key AND revision= @expected_revision
RETURNING *;

-- name: DeleteWakeupDefinition :execrows
DELETE FROM issue_wakeup_definition
WHERE workspace_id= @workspace_id AND scope_kind= @scope_kind AND scope_id= @scope_id AND rule_key= @rule_key AND revision= @expected_revision;

-- name: LockWorkspaceSettingsForWakeupDefinition :exec
-- A definition write that touches the settings aliases holds the workspace row
-- from its revision check to its commit, so a settings writer cannot slip in
-- between: every settings write is an UPDATE of this row, which takes the same
-- NO KEY UPDATE lock. That mode does not block the FOR KEY SHARE that rows
-- referencing the workspace take, as a plain FOR UPDATE would.
SELECT id FROM workspace WHERE id= @workspace_id FOR NO KEY UPDATE;

-- name: RetireCustomizedSystemWakeup :one
-- Reset an issue's legacy override to inheritance. Pauses, fire counts and
-- consumed state stay as they are: enabled follows the inherited value only on
-- a row nothing has paused or ended.
UPDATE issue_wakeup SET customized_at=NULL,instruction='',
 enabled=CASE WHEN paused_reason IS NULL AND disabled_at IS NULL THEN @enabled::bool ELSE enabled END,
 updated_at=clock_timestamp()
WHERE id= @id AND customized_at IS NOT NULL
RETURNING *;

-- name: RebaseSystemWakeupConfig :one
-- A platform rule's instance moves to the configuration it now resolves to. The
-- revision moves with it, so inputs and queued runs captured under the old
-- configuration stop matching; identity, fire count, pauses and consumed state
-- stay as they are. An empty fingerprint means no scoped definition applies.
UPDATE issue_wakeup SET revision=revision+1,config_fingerprint=NULLIF(@fingerprint::text,''),updated_at=clock_timestamp()
WHERE id= @id AND system_rule IS NOT NULL
RETURNING *;

-- name: ListIssueLabelIDs :many
SELECT label_id FROM issue_to_label WHERE issue_id= @issue_id;
