SET LOCAL lock_timeout = '2s';

-- DM attention storage (CHE-4145, plan r4). Additive and re-runnable.
-- A chain is a DM and the DMs it was continued into; control state is keyed by
-- the chain root so it reads the same from every DM of the chain.
ALTER TABLE chat_session ADD COLUMN IF NOT EXISTS chain_root_session_id uuid;

-- BEFORE INSERT keeps every create path valid until the continue path writes the
-- predecessor's root (L2): a new DM starts its own chain.
CREATE OR REPLACE FUNCTION chat_session_default_chain_root() RETURNS trigger AS $$
BEGIN
    NEW.chain_root_session_id := COALESCE(NEW.chain_root_session_id, NEW.id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_chat_session_default_chain_root ON chat_session;
CREATE TRIGGER trg_chat_session_default_chain_root
    BEFORE INSERT ON chat_session
    FOR EACH ROW EXECUTE FUNCTION chat_session_default_chain_root();

-- One-time back-fill. Walk forward from every true root (no continued_from); a cycle has no
-- root so the walk cannot loop. Rows it never reaches (cycle, missing parent) become their own
-- root. Only NULL rows are written, so a re-run changes nothing.
WITH RECURSIVE walk AS (
    SELECT id, id AS root FROM chat_session WHERE continued_from_session_id IS NULL
    UNION ALL
    SELECT s.id, w.root FROM chat_session s JOIN walk w ON s.continued_from_session_id = w.id
)
UPDATE chat_session s SET chain_root_session_id = w.root
FROM walk w WHERE s.id = w.id AND s.chain_root_session_id IS NULL;
UPDATE chat_session SET chain_root_session_id = id WHERE chain_root_session_id IS NULL;

-- What Cheese owes in a DM that is not a decision_request. kind:
--   ask    agent outcome `question`
--   later  explicit "check later", at most one open per chain and user
--   unseen explicit mark-unseen
-- No foreign keys (repo rule): workspace, user, session and task ids are soft references.
-- An item must outlive the DM it was created in when the chain is continued; workspace
-- delete removes items and marks in application code. Indexes: 975-977, built CONCURRENTLY.
CREATE TABLE IF NOT EXISTS chat_attention_item (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    chain_root_session_id uuid NOT NULL,
    chat_session_id uuid NOT NULL,
    user_id uuid NOT NULL,
    agent_id uuid,
    kind text NOT NULL CHECK (kind IN ('ask', 'later', 'unseen')),
    opened_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    due_at timestamptz,
    source_task_id uuid,
    source_message_id uuid,
    last_outcome_task_id uuid,
    waiting_message_id uuid,
    waiting_task_id uuid,
    resolved_at timestamptz,
    resolved_by text CHECK (resolved_by IN ('xong', 'human_send', 'agent_done', 'replaced')),
    resolve_batch_id uuid,
    CONSTRAINT chat_attention_item_due_iff_later CHECK ((due_at IS NOT NULL) = (kind = 'later')),
    CONSTRAINT chat_attention_item_agent_iff_ask CHECK ((agent_id IS NOT NULL) = (kind = 'ask')),
    CONSTRAINT chat_attention_item_resolved_iff CHECK ((resolved_by IS NOT NULL) = (resolved_at IS NOT NULL))
);

-- Presentation marks keyed by subject key (`item:<id>`, `decision:<id>@<rev>`,
-- `turn:<task_id>`): no session column, so a mark means the same from every DM.
CREATE TABLE IF NOT EXISTS chat_attention_mark (
    user_id uuid NOT NULL,
    subject_key text NOT NULL,
    chain_root_session_id uuid NOT NULL,
    snoozed_until timestamptz,
    acked_at timestamptz,
    ack_batch_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, subject_key)
);
