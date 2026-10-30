SET LOCAL lock_timeout = '2s';

-- One row per accepted decision-bound write. The primary key on decision_id is
-- the single-use lock; the row is also the audit record.
CREATE TABLE IF NOT EXISTS decision_bound_write (
    decision_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    field text NOT NULL,
    before_sha256 text NOT NULL,
    after_sha256 text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
