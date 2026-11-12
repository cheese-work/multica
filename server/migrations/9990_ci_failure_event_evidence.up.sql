-- CHE-4274: evidence collector columns. Defaults keep a row written by an older
-- binary readable as a partial event with no excerpts.
ALTER TABLE ci_failure_event ADD COLUMN IF NOT EXISTS evidence text NOT NULL DEFAULT 'partial';
ALTER TABLE ci_failure_event ADD COLUMN IF NOT EXISTS partial_reason text NOT NULL DEFAULT '';
ALTER TABLE ci_failure_event ADD COLUMN IF NOT EXISTS failed_job_count integer NOT NULL DEFAULT 0;
-- Redacted excerpts only: [{job_id, name, excerpt}], at most 2 jobs, 8 KB each.
ALTER TABLE ci_failure_event ADD COLUMN IF NOT EXISTS excerpts jsonb NOT NULL DEFAULT '[]';
