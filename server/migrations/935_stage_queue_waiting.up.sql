-- CHE-3611: a PR that a full stage queue refused. The release job starts it
-- when a slot frees. A later task of the same agent on the issue satisfies the
-- row, so it needs no cleanup on completion.
CREATE TABLE stage_queue_waiting (
    issue_id  UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    agent_id  UUID NOT NULL REFERENCES agent(id) ON DELETE CASCADE,
    queued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, agent_id)
);
