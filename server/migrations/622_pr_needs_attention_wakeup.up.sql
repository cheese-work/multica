-- CHE-1417: the pr_needs_attention system rule wakes an issue's owner when a
-- linked PR can no longer merge as is (DIRTY/BEHIND) or returns to draft after
-- the issue was accepted. snapshot_base_ref is the base branch the API
-- snapshot's merge state was computed against, so a retarget is a new
-- (head, base) pair. A nullable column with no default is catalog-only.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE github_pull_request ADD COLUMN IF NOT EXISTS snapshot_base_ref TEXT;

ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
 CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed', 'pr_needs_attention'));

ALTER TABLE issue_wakeup_definition DROP CONSTRAINT issue_wakeup_definition_rule_key_check;
ALTER TABLE issue_wakeup_definition ADD CONSTRAINT issue_wakeup_definition_rule_key_check
 CHECK (rule_key IN ('child_done','pr_merged','pr_checks_failed','pr_needs_attention')
  OR rule_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');
ALTER TABLE issue_wakeup_definition DROP CONSTRAINT issue_wakeup_definition_root_check;
ALTER TABLE issue_wakeup_definition ADD CONSTRAINT issue_wakeup_definition_root_check
 CHECK (NOT root OR (scope_kind <> 'issue' AND rule_key NOT IN ('child_done','pr_merged','pr_checks_failed','pr_needs_attention')));
