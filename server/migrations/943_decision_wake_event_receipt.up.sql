SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request ADD COLUMN wake_seq bigint NOT NULL DEFAULT 0;

CREATE TABLE decision_wake_event (
    id uuid PRIMARY KEY,
    request_id uuid NOT NULL,
    seq bigint NOT NULL,
    type text NOT NULL CHECK (type IN ('answered', 'retracted', 'expired', 'withdrawn_human', 'withdrawn_agent', 'reask', 'requester_archived')),
    answer_id uuid,
    source_comment_id uuid,
    superseded_at timestamptz,
    superseded_reason text CHECK (superseded_reason IN ('answer_disclaimed')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE decision_wake_receipt (
    id uuid PRIMARY KEY,
    event_id uuid NOT NULL,
    event_seq bigint NOT NULL,
    recipient_agent_id uuid NOT NULL,
    role text NOT NULL CHECK (role IN ('requester', 'assignee')),
    squad_id uuid,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'delivered', 'skipped_ineligible', 'superseded', 'suppressed_issue_closed')),
    outcome text CHECK (outcome IN ('merged_into_queued', 'planned_input', 'run_enqueued')),
    attempt_count integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    last_error text,
    delivered_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
