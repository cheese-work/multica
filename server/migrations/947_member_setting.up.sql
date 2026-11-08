-- Generic per-member settings (CHE-4111). Key `background_agents` holds a
-- member's own background-job agent chain. Reads are scoped to the caller.
CREATE TABLE member_setting (
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    key TEXT NOT NULL,
    value JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id, key)
);
