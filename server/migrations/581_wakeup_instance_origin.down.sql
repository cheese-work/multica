ALTER TABLE issue_wakeup
 DROP CONSTRAINT IF EXISTS issue_wakeup_default_origin_check,
 DROP CONSTRAINT IF EXISTS issue_wakeup_default_scope_kind_check,
 DROP COLUMN IF EXISTS config_fingerprint,
 DROP COLUMN IF EXISTS default_scope_id,
 DROP COLUMN IF EXISTS default_scope_kind,
 DROP COLUMN IF EXISTS default_rule_key;
