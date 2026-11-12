DROP TRIGGER IF EXISTS issue_description_revision_upd ON issue;
DROP TRIGGER IF EXISTS issue_description_revision_ins ON issue;
DROP FUNCTION IF EXISTS record_issue_description_revision();
DROP TABLE IF EXISTS issue_description_revision;
