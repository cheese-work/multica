-- Exact-snapshot approval of an `unsigned` GitHub revision (CHE-2126 plan 5.3, R3).
-- One approval per revision; a later change to the object is a new revision and
-- needs its own. Holds hashes and IDs only, never GitHub text. No foreign keys
-- (repo rule): workspace delete removes these rows with the revisions.
CREATE TABLE github_snapshot_approval (
    workspace_id     UUID        NOT NULL,
    revision_id      UUID        NOT NULL,
    approver_user_id UUID        NOT NULL,
    snapshot_hash    TEXT        NOT NULL CHECK (snapshot_hash ~ '^[0-9a-f]{64}$'),
    object_kind      TEXT        NOT NULL,
    object_id        BIGINT      NOT NULL,
    author_id        BIGINT      NOT NULL,
    body_hash        TEXT        NOT NULL CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    review_state     TEXT        NOT NULL,
    editor_type      TEXT        NOT NULL,
    editor_id        BIGINT      NOT NULL,
    last_edited_at   TIMESTAMPTZ,
    read_at          TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, revision_id)
);
