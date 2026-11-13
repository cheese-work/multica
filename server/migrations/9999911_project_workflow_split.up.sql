-- CHE-5262: per-project A/B split of the project's workflow (CHE-5238).
-- NULL = no split. Additive; the down migration drops it.
ALTER TABLE project ADD COLUMN workflow_split jsonb;
