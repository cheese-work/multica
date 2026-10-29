-- CHE-2439: the head a head_changed event replaced, read from the snapshot
-- before it was overwritten. Empty for every other kind.
ALTER TABLE issue_pr_event ADD COLUMN IF NOT EXISTS previous_head_sha TEXT NOT NULL DEFAULT '';
