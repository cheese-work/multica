-- Outbox for the effective-record notice. The row is written in the same
-- transaction as the settlement; the decision worker delivers it after commit
-- and retries until delivered_at is set. A failed attempt stays visible to the
-- brief through attempts and last_error.
CREATE TABLE decision_agent_notice (
    instance_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    delivered_at timestamptz
);
