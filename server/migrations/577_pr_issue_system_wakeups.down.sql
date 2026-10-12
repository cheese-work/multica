DELETE FROM issue_wakeup_receipt
WHERE wakeup_id IN (SELECT id FROM issue_wakeup WHERE system_rule IN ('pr_merged', 'pr_checks_failed'));
DELETE FROM issue_wakeup WHERE system_rule IN ('pr_merged', 'pr_checks_failed');
ALTER TABLE issue_wakeup DROP CONSTRAINT issue_wakeup_system_rule_check;
ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_system_rule_check CHECK (system_rule IN ('child_done'));
