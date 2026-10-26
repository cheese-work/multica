SET LOCAL lock_timeout = '2s';

DROP TRIGGER IF EXISTS issue_run_budget_reason_clear ON issue;
DROP TRIGGER IF EXISTS issue_workflow_edges_update ON issue;
DROP TRIGGER IF EXISTS issue_workflow_edges_insert ON issue;
DROP FUNCTION IF EXISTS clear_issue_run_budget_reason();
DROP FUNCTION IF EXISTS sync_issue_workflow_edges();
DELETE FROM issue_wakeup WHERE system_rule = 'workflow_edges';
