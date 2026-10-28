-- A per-issue run cap a person raised above the workflow's (CHE-2226).
-- Written only by the member-only raise endpoint; null means the workflow's cap.
ALTER TABLE issue ADD COLUMN max_runs_override INTEGER;
