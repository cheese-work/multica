SET LOCAL lock_timeout = '2s';

-- A sticky step assignee (CHE-2299 S4): who an issue goes to whenever it
-- re-enters a status, kept until cleared. Presence of the row is the state.
-- No foreign keys (workspace DB rules): issue_id and assignee_id are checked
-- in application code.
CREATE TABLE IF NOT EXISTS issue_step_assignee (
    issue_id uuid NOT NULL,
    status_key text NOT NULL,
    workspace_id uuid NOT NULL,
    assignee_type text NOT NULL CHECK (assignee_type IN ('member', 'agent', 'squad')),
    assignee_id uuid NOT NULL,
    set_by_type text NOT NULL,
    set_by_id uuid,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, status_key)
);
