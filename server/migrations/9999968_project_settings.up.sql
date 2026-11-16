-- CHE-5907 (C3): generic per-project settings, the project tier of comment_wake
-- (issue metadata > project settings > workspace settings). Same shape as
-- workspace.settings. Additive; NOT NULL with a constant default, so no rewrite
-- risk beyond a metadata-only change on a small table. The down migration drops it.
ALTER TABLE project ADD COLUMN settings jsonb NOT NULL DEFAULT '{}'::jsonb;
