-- Append-only record of every save that went through despite lint errors, so
-- the reason outlives the next save of the workflow. version is the saved
-- workflow's updated_at. No foreign keys: the workflow id is checked in code.
CREATE TABLE IF NOT EXISTS issue_workflow_lint_override (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    workflow_id uuid NOT NULL,
    version timestamptz NOT NULL,
    reason text NOT NULL,
    actor_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
