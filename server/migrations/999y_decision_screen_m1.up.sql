SET LOCAL lock_timeout = '2s';

-- Decision screen M1 (CHE-5480 plan r4, CHE-5995). Additive: nullable columns, new
-- tables, widened CHECKs. Indexes on the new tables come as single-statement
-- concurrent-build files (999z here, later slices for the rest). No foreign keys.

-- default_apply: the server's own answer for an auto-applied ask. No human, task or source.
-- Widens the 873 definition. NOT VALID then VALIDATE keeps the table lock short.
ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_source_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_source_check CHECK (
    (answer_source = 'agent_record' AND answered_by_user_id IS NULL AND (
        (source_comment_id IS NULL AND source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)
        OR (source_comment_id IS NOT NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)))
    OR (answer_source = 'card' AND answered_by_user_id IS NOT NULL AND answered_by_task_id IS NULL AND source_comment_id IS NULL
        AND ((source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)
            OR (source_chat_session_id IS NOT NULL AND cardinality(chat_message_ids) > 0)))
    OR (answer_source = 'default_apply' AND answered_by_user_id IS NULL AND answered_by_task_id IS NULL
        AND source_comment_id IS NULL AND source_chat_session_id IS NULL AND cardinality(chat_message_ids) = 0)
) NOT VALID;
ALTER TABLE decision_answer VALIDATE CONSTRAINT decision_answer_source_check;

-- undo: a card answer withdrawn inside the undo grace. Widens the 871 definition.
ALTER TABLE decision_answer DROP CONSTRAINT IF EXISTS decision_answer_disclaim_reason_check;
ALTER TABLE decision_answer ADD CONSTRAINT decision_answer_disclaim_reason_check
    CHECK (disclaim_reason IN ('human', 'answering_turn_cancelled', 'answering_turn_failed', 'source_removed', 'agent_veto', 'record_retracted', 'class_needs_owner', 'card_required', 'undo')) NOT VALID;
ALTER TABLE decision_answer VALIDATE CONSTRAINT decision_answer_disclaim_reason_check;

ALTER TABLE decision_answer ADD COLUMN IF NOT EXISTS undo_until timestamptz;

-- NULL on old rows.
ALTER TABLE decision_request
    ADD COLUMN IF NOT EXISTS route text CHECK (route IN ('auto_apply', 'human')),
    ADD COLUMN IF NOT EXISTS would_auto_apply boolean,
    ADD COLUMN IF NOT EXISTS auto_apply_rule_id uuid;

-- A member's hidden chat session for issue-agent asks (plan 3.7). The unique index is 999z.
CREATE TABLE IF NOT EXISTS decision_backing_lane (
    session_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    creator_id uuid NOT NULL,
    topic_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Blind-pick agreement data (plan 3.3). A mismatch is data, not an error.
CREATE TABLE IF NOT EXISTS decision_pick (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    decision_request_id uuid NOT NULL,
    revision integer NOT NULL,
    member_id uuid NOT NULL,
    first_shown_at timestamptz NOT NULL,
    picked_at timestamptz,
    read_ms integer,
    matched boolean,
    surface text NOT NULL
);

-- Snooze (plan 3.6): ends within 24 h; the condition is an issue and a status, both or neither.
CREATE TABLE IF NOT EXISTS decision_snooze (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    member_id uuid NOT NULL,
    decision_request_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    until_at timestamptz NOT NULL,
    cond_issue_id uuid,
    cond_status text,
    cleared_at timestamptz,
    clear_reason text,
    CONSTRAINT decision_snooze_until_cap_check CHECK (until_at <= created_at + interval '24 hours'),
    CONSTRAINT decision_snooze_cond_pair_check CHECK ((cond_issue_id IS NULL) = (cond_status IS NULL))
);
