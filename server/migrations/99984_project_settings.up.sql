-- Generic per-project settings (CHE-5213). Key `wip_limit` (integer >= 1) caps
-- the runs a project has dispatched, running or reserved at once; absent means
-- unlimited. One JSON column, not one column per setting.
ALTER TABLE project ADD COLUMN IF NOT EXISTS settings jsonb NOT NULL DEFAULT '{}'::jsonb;
