DROP TRIGGER IF EXISTS merge_receipt_invalidate ON github_pull_request;
DROP FUNCTION IF EXISTS merge_receipt_invalidate();
DROP TABLE IF EXISTS merge_receipt_fact;
DROP TABLE IF EXISTS merge_veto;
DROP TRIGGER IF EXISTS comment_content_edit_record ON comment;
DROP FUNCTION IF EXISTS comment_content_edit_record();
DROP TABLE IF EXISTS comment_content_edit;
