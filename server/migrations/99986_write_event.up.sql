-- CHE-5213: one row per platform write that carries a stable event id (comment,
-- status change, wakeup). A redelivered input replays the stored response
-- instead of writing again. No foreign keys; rows are cleaned by age.
--
-- owner is the request that holds the claim. The write's own transaction
-- flips state to 'applied' while it holds the claim, so the marker commits
-- with the write or not at all: a stale 'pending' claim never committed, an
-- 'applied' one did.
CREATE TABLE IF NOT EXISTS write_event (
    workspace_id UUID NOT NULL,
    event_key TEXT NOT NULL,
    owner UUID NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'applied', 'done')),
    status_code INTEGER,
    response_body BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, event_key)
);
