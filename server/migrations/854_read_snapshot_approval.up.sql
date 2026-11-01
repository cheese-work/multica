-- Scoped snapshot approval for elevated reads (CHE-2216 plan 3.4, 3.7, slice B3):
-- a member approves the exact bytes (by SHA-256) of one object for one issue,
-- for objects that have no admission path (PR title and body, commit message, CI
-- log) and for Multica bodies written by a run that read external text. Holds
-- hashes and IDs only, never body text. No foreign keys (repo rule): workspace
-- delete removes these rows with the issue.
CREATE TABLE read_snapshot_approval (
    workspace_id     UUID        NOT NULL,
    issue_id         UUID        NOT NULL,
    object_kind      TEXT        NOT NULL CHECK (object_kind IN ('pr_title', 'pr_body', 'commit_message', 'ci_log', 'multica_comment', 'multica_description')),
    object_id        TEXT        NOT NULL CHECK (object_id <> ''),
    body_hash        TEXT        NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    approver_user_id UUID        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, issue_id, object_kind, object_id, body_hash)
);
