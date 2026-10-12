-- Instance origin metadata for a default-derived issue_wakeup: the rule key,
-- the workspace/project scope that defines it, and the effective-config
-- fingerprint it was last captured under. All NULL for a local wakeup and for
-- every existing row. Nothing sets these yet; capacity accounting that depends
-- on them arrives with the layer that creates default-derived instances.
ALTER TABLE issue_wakeup
 ADD COLUMN IF NOT EXISTS default_rule_key text,
 ADD COLUMN IF NOT EXISTS default_scope_kind text,
 ADD COLUMN IF NOT EXISTS default_scope_id uuid,
 ADD COLUMN IF NOT EXISTS config_fingerprint text;
ALTER TABLE issue_wakeup
 ADD CONSTRAINT issue_wakeup_default_scope_kind_check CHECK (default_scope_kind IN ('workspace','project')),
 ADD CONSTRAINT issue_wakeup_default_origin_check CHECK (
  (default_rule_key IS NULL) = (default_scope_kind IS NULL) AND (default_scope_kind IS NULL) = (default_scope_id IS NULL));
