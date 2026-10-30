DROP TRIGGER IF EXISTS decision_missing_request ON decision_request;
DROP TRIGGER IF EXISTS decision_missing_issue ON issue;
DROP FUNCTION IF EXISTS track_decision_missing_wait();
DROP FUNCTION IF EXISTS sync_decision_missing_wait(uuid);
