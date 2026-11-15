-- CHE-3113: a workflow can name the status where its new issues start. NULL
-- keeps today's behavior (todo). The step must exist in the workflow and sit
-- in an unstarted or started category; the write path checks both.
ALTER TABLE issue_workflow
    ADD COLUMN IF NOT EXISTS start_status_key text;
