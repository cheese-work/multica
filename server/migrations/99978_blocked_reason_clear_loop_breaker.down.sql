SET LOCAL lock_timeout = '2s';

DROP TRIGGER IF EXISTS issue_run_budget_reason_clear ON issue;

CREATE TRIGGER issue_run_budget_reason_clear
    BEFORE UPDATE OF status ON issue
    FOR EACH ROW WHEN (OLD.status = 'blocked' AND NEW.status IS DISTINCT FROM 'blocked'
                       AND OLD.metadata ->> 'blocked_reason' IN ('run-budget', 'decision-dispatch-refused'))
    EXECUTE FUNCTION clear_issue_run_budget_reason();
