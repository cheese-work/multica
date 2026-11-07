SET LOCAL lock_timeout = '2s';

-- CHE-2856: taint IDs a member cleared for an issue that a tainted task still
-- carried when it assigned the issue. The issue stays clean (the clear stands),
-- but a run started by the assignment before that task's refusal callback adds
-- its taint must not be elevated. Read only by the budget recovery run. No
-- foreign keys (repo rule): the handler validates the issue.
CREATE TABLE IF NOT EXISTS issue_run_taint (
    issue_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    taint_ids text[] NOT NULL DEFAULT '{}'
);
