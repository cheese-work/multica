SET LOCAL lock_timeout = '2s';

DELETE FROM issue_wakeup_definition WHERE rule_key = 'pr_needs_attention';
ALTER TABLE issue_wakeup_definition DROP CONSTRAINT issue_wakeup_definition_root_check;
ALTER TABLE issue_wakeup_definition ADD CONSTRAINT issue_wakeup_definition_root_check
 CHECK (NOT root OR (scope_kind <> 'issue' AND rule_key NOT IN ('child_done','pr_merged','pr_checks_failed')));
ALTER TABLE issue_wakeup_definition DROP CONSTRAINT issue_wakeup_definition_rule_key_check;
ALTER TABLE issue_wakeup_definition ADD CONSTRAINT issue_wakeup_definition_rule_key_check
 CHECK (rule_key IN ('child_done','pr_merged','pr_checks_failed')
  OR rule_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');

DELETE FROM issue_wakeup_pr_event
WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE system_rule = 'pr_needs_attention');
DELETE FROM issue_wakeup_receipt
WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE system_rule = 'pr_needs_attention');
DELETE FROM issue_wakeup WHERE system_rule = 'pr_needs_attention';
ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
 CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed'));

ALTER TABLE github_pull_request DROP COLUMN IF EXISTS snapshot_base_ref;
