-- agent_preset: a named bundle of run settings an agent can switch to. A NULL
-- column inherits the agent row. agent_id has no foreign key; the app deletes
-- presets when an agent is hard-deleted, and archive keeps them.
CREATE TABLE agent_preset (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    name TEXT NOT NULL,
    model TEXT,
    thinking_level TEXT,
    service_tier TEXT,
    context_args JSONB,
    is_default BOOLEAN NOT NULL DEFAULT false,
    created_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
