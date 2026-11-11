-- CHE-5213: one row per platform write that carries a stable event id (comment,
-- status change, wakeup). A redelivered input replays the stored response
-- instead of writing again. No foreign keys; rows are cleaned by age.
CREATE TABLE IF NOT EXISTS write_event (
    workspace_id UUID NOT NULL,
    event_key TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'done')),
    status_code INTEGER,
    response_body BYTEA,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, event_key)
);
