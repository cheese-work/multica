SET LOCAL lock_timeout = '2s';

-- A new comment (plan rev-N) supersedes older comment threads (rev-N-1 and its
-- verdicts). One row per superseded thread root; the root is superseded once, by
-- the latest comment that names it. No foreign keys (repo rule): the handler
-- validates both comments and the shared issue.
CREATE TABLE IF NOT EXISTS comment_supersession (
    superseded_comment_id uuid PRIMARY KEY,
    superseding_comment_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
