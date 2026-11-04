CREATE TABLE decision_agent_settlement (
    instance_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    effective_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    revoked_by uuid,
    revoke_reason text
);

CREATE TABLE decision_agent_reliance (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    instance_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    task_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    purpose text NOT NULL CHECK (length(btrim(purpose)) BETWEEN 1 AND 4096),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- No live writer is enabled in C2. C3 will replace the settlement trigger to
-- permit one-way revocation; neither table permits history rewriting.
CREATE TRIGGER agent_decision_settlement_immutable BEFORE UPDATE OR DELETE ON decision_agent_settlement
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_scope();
CREATE TRIGGER agent_decision_reliance_immutable BEFORE UPDATE OR DELETE ON decision_agent_reliance
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_scope();
