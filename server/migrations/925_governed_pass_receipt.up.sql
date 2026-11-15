SET LOCAL lock_timeout = '2s';

-- CHE-3490: one row per PASS receipt that authorized an agent write of a
-- governed field. The primary key on comment_id is the single-use lock; the
-- row is also the audit record.
CREATE TABLE IF NOT EXISTS governed_pass_receipt (
    comment_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    reviewer_id uuid NOT NULL,
    writer_id uuid NOT NULL,
    field text NOT NULL,
    target_id uuid NOT NULL,
    before_sha256 text NOT NULL,
    after_sha256 text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
