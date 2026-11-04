-- CHE-3611: a PR that a full stage queue refused. The release job starts it
-- when a slot frees. A later task of the same agent on the issue satisfies the
-- row, so it needs no cleanup on completion.
-- A refused PR also keeps who asked for the run, the comment that asked, and
-- the taint the asker carried. The release job starts the run from them, so
-- attribution and taint match the refused run, not the issue's creator with a
-- clean record. Those three columns carry no foreign keys.
CREATE TABLE stage_queue_waiting (
    issue_id           UUID NOT NULL REFERENCES issue(id) ON DELETE CASCADE,
    agent_id           UUID NOT NULL REFERENCES agent(id) ON DELETE CASCADE,
    queued_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_user_id      UUID,
    trigger_comment_id UUID,
    taint_ids          text[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (issue_id, agent_id)
);
