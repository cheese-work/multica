-- DM wakeup (CHE-1874): an agent in a web DM schedules its own follow-up turn.
-- chat_session_id, agent_id and the rest carry no foreign keys; DM deletion and
-- workspace teardown remove the rows in application code. created_by is the
-- DM's creator, who is accountable for the turn the wakeup starts.
CREATE TABLE chat_wakeup (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL,
    chat_session_id UUID NOT NULL,
    agent_id        UUID NOT NULL,
    created_by      UUID NOT NULL,
    source_task_id  UUID,
    instruction     TEXT NOT NULL CHECK (octet_length(instruction) BETWEEN 1 AND 12000),
    fire_at         TIMESTAMPTZ NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'fired', 'cancelled', 'expired')),
    task_id         UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
