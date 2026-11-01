ALTER TABLE issue_wakeup ADD COLUMN IF NOT EXISTS served_revisions bigint[] NOT NULL DEFAULT '{}';
ALTER TABLE issue_wakeup DROP COLUMN IF EXISTS content_revision;
