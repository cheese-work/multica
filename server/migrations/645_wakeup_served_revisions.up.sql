-- CHE-1082 L10. The earlier revisions of a default-derived instance whose
-- configuration a rebase left unchanged (only the fingerprint moved: the issue's
-- assignee, say). Live capture stamps an outbox input with id:revision of the
-- instance it served; a stamp at one of these revisions still means "served" for
-- the current configuration, in whichever transaction the input is reached. A
-- rebase that changes what the instance runs empties the list. Bounded to the
-- most recent 32 by the writer.
ALTER TABLE issue_wakeup ADD COLUMN IF NOT EXISTS served_revisions bigint[] NOT NULL DEFAULT '{}';
