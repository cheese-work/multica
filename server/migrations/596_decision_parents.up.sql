CREATE TABLE IF NOT EXISTS decision_request (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    asking_agent_id uuid NOT NULL,
    human_user_id uuid NOT NULL,
    chat_session_id uuid NOT NULL,
    ping_message_id uuid,
    issue_comment_id uuid,
    ask text NOT NULL DEFAULT '',
    escalation_class text NOT NULL CHECK (escalation_class IN ('reversible', 'spend', 'deploy', 'publish', 'irreversible', 'product_direction')),
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'answered', 'disclaimed', 'cancelled')),
    origin text NOT NULL CHECK (origin IN ('asked', 'unprompted')),
    decision_intent_id uuid,
    cancel_reason text CHECK (cancel_reason IN ('issue_closed', 'human_removed', 'agent_archived', 'lane_archived', 'record_disclaimed', 'record_vetoed')),
    cancel_notice_posted_at timestamptz,
    ping_notified_at timestamptz,
    reask_delivered_at timestamptz,
    reask_outcome text CHECK (reask_outcome IN ('run_enqueued', 'merged_into_queued', 'planned_input', 'comment_member', 'comment_unassigned', 'skipped_ineligible')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS decision_intent (
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    task_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    comment_id uuid NOT NULL,
    opened_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    consumed_by_request_id uuid,
    closed_at timestamptz,
    close_reason text CHECK (close_reason IN ('superseded', 'comment_deleted')),
    sealed_at timestamptz,
    group_woken_at timestamptz
);

ALTER TABLE comment
    ADD COLUMN IF NOT EXISTS decision_request_id uuid,
    ADD COLUMN IF NOT EXISTS decision_answer_id uuid,
    ADD COLUMN IF NOT EXISTS decision_intent_id uuid,
    ADD COLUMN IF NOT EXISTS decision_comment_kind text CHECK (decision_comment_kind IN ('ask', 'quote', 'disclaim', 'delivery', 'cancel', 'veto'));
