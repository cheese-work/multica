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
