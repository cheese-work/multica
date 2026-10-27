-- chat_wakeup: a one-shot scheduled turn an agent asks for inside its own DM.
-- At fire_at the server sends the instruction into the DM as a labelled turn.
CREATE TABLE chat_wakeup (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    chat_session_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    instruction TEXT NOT NULL,
    fire_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'fired', 'cancelled')),
    created_by_task_id UUID,
    fired_task_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at TIMESTAMPTZ
);
