CREATE TABLE IF NOT EXISTS decision_answer (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    decision_request_id uuid NOT NULL REFERENCES decision_request (id) ON DELETE CASCADE,
    answered_by_task_id uuid NOT NULL,
    source_chat_session_id uuid NOT NULL,
    chat_message_ids uuid[] NOT NULL CHECK (cardinality(chat_message_ids) > 0),
    content_snapshot text NOT NULL,
    effective_at timestamptz NOT NULL,
    wake_at timestamptz,
    woken_at timestamptz,
    wake_outcome text CHECK (wake_outcome IN ('run_enqueued', 'merged_into_queued', 'planned_input', 'comment_member', 'comment_unassigned', 'skipped_ineligible')),
    issue_comment_id uuid,
    disclaimed_at timestamptz,
    disclaim_comment_posted_at timestamptz,
    disclaimed_by_user_id uuid,
    disclaim_reason text CHECK (disclaim_reason IN ('human', 'answering_turn_cancelled', 'answering_turn_failed', 'source_removed', 'agent_veto', 'record_retracted')),
    disclaimed_by_agent_id uuid,
    disclaimed_by_task_id uuid,
    veto_reason text CHECK (char_length(veto_reason) <= 600),
    veto_note_posted_at timestamptz,
    retraction_delivered_at timestamptz,
    retraction_outcome text CHECK (retraction_outcome IN ('run_enqueued', 'merged_into_queued', 'planned_input', 'comment_member', 'comment_unassigned', 'skipped_ineligible', 'suppressed_agent_veto')),
    withdrawal_comment_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS decision_intent_target (
    decision_intent_id uuid NOT NULL REFERENCES decision_intent (id) ON DELETE CASCADE,
    human_user_id uuid NOT NULL,
    consumed_at timestamptz,
    consumed_by_request_id uuid
);

CREATE TABLE IF NOT EXISTS decision_class_change (
    decision_request_id uuid NOT NULL REFERENCES decision_request (id) ON DELETE CASCADE,
    from_class text NOT NULL CHECK (from_class IN ('reversible', 'spend', 'deploy', 'publish', 'irreversible', 'product_direction')),
    to_class text NOT NULL CHECK (to_class IN ('reversible', 'spend', 'deploy', 'publish', 'irreversible', 'product_direction')),
    changed_by_user_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS decision_worker_lease (
    name text NOT NULL,
    holder text NOT NULL,
    token uuid NOT NULL,
    expires_at timestamptz NOT NULL
);
