-- CHE-1082 L10. content_revision is the first revision of the configuration a
-- default-derived instance runs now. Every revision from content_revision up to
-- the current one ran that same configuration: a rebase that changes only the
-- fingerprint (the issue's assignee, say) keeps it, a rebase that changes what the
-- instance runs sets it to the new revision. Live capture stamps an outbox input
-- with instance:revision, so a stamp inside that range means "served" for the
-- instance as it is now, in whichever transaction the input is reached, however
-- many rebases happened since. 0 reads as 1 (an instance that never changed).
-- It replaces the bounded list added by 645, which was never released.
ALTER TABLE issue_wakeup ADD COLUMN IF NOT EXISTS content_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE issue_wakeup DROP COLUMN IF EXISTS served_revisions;
