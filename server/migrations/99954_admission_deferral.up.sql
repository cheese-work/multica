-- CHE-4650 (CHE-4615 S2): a reviewer run the server did not start because an
-- admission check on the PR head was failing, pending or missing, or the PR
-- was DIRTY. One live record per (issue, agent): a deferral on a new head
-- replaces the record of the old head. expires_at bounds the wait to 24 h.
-- Actor, trigger comment and taint keep the refused run's attribution, so a
-- later start (CHE-4651) is never cleaner or more privileged than the refusal.
-- No foreign keys (repo rule): a row for a deleted issue or agent is inert and
-- ages out at expires_at.
CREATE TABLE admission_deferral (
    issue_id           UUID NOT NULL,
    agent_id           UUID NOT NULL,
    trigger            TEXT NOT NULL,
    head_sha           TEXT NOT NULL,
    reasons            TEXT[] NOT NULL,
    actor_user_id      UUID,
    trigger_comment_id UUID,
    taint_ids          TEXT[] NOT NULL DEFAULT '{}',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at         TIMESTAMPTZ NOT NULL DEFAULT now() + interval '24 hours',
    PRIMARY KEY (issue_id, agent_id)
);
