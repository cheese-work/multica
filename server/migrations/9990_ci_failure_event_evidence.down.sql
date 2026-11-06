ALTER TABLE ci_failure_event DROP COLUMN IF EXISTS excerpts;
ALTER TABLE ci_failure_event DROP COLUMN IF EXISTS failed_job_count;
ALTER TABLE ci_failure_event DROP COLUMN IF EXISTS partial_reason;
ALTER TABLE ci_failure_event DROP COLUMN IF EXISTS evidence;
