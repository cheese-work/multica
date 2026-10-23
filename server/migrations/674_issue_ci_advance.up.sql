-- One row per issue and PR head whose green checks already advanced
-- the issue one workflow step, so a redelivered snapshot never advances it again.
CREATE TABLE IF NOT EXISTS issue_ci_advance (
    issue_id UUID NOT NULL,
    head_sha TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
