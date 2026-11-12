-- CHE-4650: a reviewer run held back because the PR head is not admissible
-- (an admission check is failing, pending or missing, or the PR has a merge
-- conflict). One row per (issue, agent); a repeat deferral overwrites it. The
-- row expires 24 h after it is written. No foreign keys, as for any new table.
CREATE TABLE review_admission_deferral (
    issue_id   UUID        NOT NULL,
    agent_id   UUID        NOT NULL,
    trigger    TEXT        NOT NULL,
    head_sha   TEXT        NOT NULL,
    reasons    TEXT[]      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
