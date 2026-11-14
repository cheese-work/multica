DROP TRIGGER IF EXISTS merge_receipt_invalidate ON github_pull_request;
DROP FUNCTION IF EXISTS merge_receipt_invalidate();
DROP TABLE IF EXISTS merge_receipt_fact;
