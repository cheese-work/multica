-- Scoped wakeup definitions: one sparse, versioned configuration patch per
-- (workspace, scope, rule). A definition is not an executable wakeup; issue_wakeup
-- rows stay each issue's runtime instance. Identity is the unique index added
-- by 582; this table has no primary key, foreign key or cascade. root marks the
-- definition that created a custom rule, so an override whose root was deleted
-- is retired rather than promoted. Nothing writes this table until definition
-- writes are opened by a later layer.
CREATE TABLE issue_wakeup_definition (
 workspace_id uuid NOT NULL,
 scope_kind text NOT NULL,
 scope_id uuid NOT NULL,
 rule_key text NOT NULL,
 root boolean NOT NULL DEFAULT false,
 config jsonb NOT NULL,
 revision bigint NOT NULL DEFAULT 1,
 created_by uuid,
 updated_by uuid,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CONSTRAINT issue_wakeup_definition_scope_kind_check CHECK (scope_kind IN ('workspace','project','issue')),
 CONSTRAINT issue_wakeup_definition_rule_key_check CHECK (rule_key IN ('child_done','pr_merged','pr_checks_failed')
  OR rule_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
 CONSTRAINT issue_wakeup_definition_workspace_scope_check CHECK (scope_kind <> 'workspace' OR scope_id = workspace_id),
 CONSTRAINT issue_wakeup_definition_root_check CHECK (NOT root OR (scope_kind <> 'issue' AND rule_key NOT IN ('child_done','pr_merged','pr_checks_failed'))),
 CONSTRAINT issue_wakeup_definition_config_check CHECK (jsonb_typeof(config) = 'object' AND COALESCE(jsonb_typeof(config->'v'), '') = 'number'),
 CONSTRAINT issue_wakeup_definition_revision_check CHECK (revision >= 1)
);
