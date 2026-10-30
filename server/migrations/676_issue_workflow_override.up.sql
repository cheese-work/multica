-- Per-issue workflow override (CHE-1986). Null means the issue inherits its
-- project's workflow. No foreign key: the handler validates the reference.
ALTER TABLE issue ADD COLUMN IF NOT EXISTS workflow_id UUID;
