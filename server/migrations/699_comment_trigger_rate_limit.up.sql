CREATE TABLE comment_trigger_rate_limit (
    issue_id UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    agent_id UUID NOT NULL REFERENCES agent(id) ON DELETE CASCADE,
    triggered_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
