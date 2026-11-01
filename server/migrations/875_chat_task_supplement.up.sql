-- Delivery receipts for a DM follow-up the session creator steered into the
-- running chat task. The issue table task_supplement keeps its NOT NULL
-- issue/comment guarantees; this table is additive and rolls back by dropping.
-- No foreign keys: message, task and session teardown stay application-owned.
--
-- status: pending = never claimed; delivering = claimed, ack unconfirmed;
-- delivered = ack received; uncertain = claimed but no positive ack (the
-- provider may hold the text); failed = never claimed when the turn ended.
CREATE TABLE chat_task_supplement (
    message_id UUID NOT NULL,
    task_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    chat_session_id UUID NOT NULL,
    author_id UUID NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'delivering', 'delivered', 'uncertain', 'failed')),
    failure_reason TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    resent_task_id UUID,
    resent_uncertain BOOLEAN NOT NULL DEFAULT false,
    acked_after_resend_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    PRIMARY KEY (message_id, task_id)
);

-- A chat run records its negotiated capability in the same table as an issue
-- run. Daemons start tasks continuously, so fail fast rather than queue their
-- starts behind a pending ACCESS EXCLUSIVE lock.
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '10s';

ALTER TABLE task_supplement_capability
    ALTER COLUMN issue_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS chat_session_id UUID;
