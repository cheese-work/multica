SET LOCAL lock_timeout = '2s';

DELETE FROM issue_wakeup_pr_event
WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE system_rule = 'pr_checks_passed');
DELETE FROM issue_wakeup_receipt
WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE system_rule = 'pr_checks_passed');
DELETE FROM issue_wakeup WHERE system_rule = 'pr_checks_passed';
ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check
 CHECK (system_rule IN ('child_done', 'pr_merged', 'pr_checks_failed', 'pr_needs_attention'));
