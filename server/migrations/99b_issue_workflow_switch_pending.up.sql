SET LOCAL lock_timeout = '2s';

-- A workflow switch that waits for the issue's last live task to end
-- (CHE-7669). At most one row per issue; the row is the state. Only the server
-- writes it, so an agent cannot forge a target through issue metadata. No
-- foreign keys (workspace DB rules): the issue and the workflow are checked in
-- application code when the switch applies.
CREATE TABLE IF NOT EXISTS issue_workflow_switch_pending (
    issue_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
