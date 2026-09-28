CREATE TABLE governance_budget_window (
    workspace_id UUID NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    spend_cap_micro_usd BIGINT NOT NULL CHECK (spend_cap_micro_usd > 0),
    reserved_micro_usd BIGINT NOT NULL DEFAULT 0 CHECK (reserved_micro_usd >= 0),
    spent_micro_usd BIGINT NOT NULL DEFAULT 0 CHECK (spent_micro_usd >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, window_start),
    CHECK (window_end > window_start)
);

CREATE TABLE governance_budget_root (
    workspace_id UUID NOT NULL,
    budget_root_id UUID NOT NULL,
    spend_cap_micro_usd BIGINT NOT NULL CHECK (spend_cap_micro_usd > 0),
    reserved_micro_usd BIGINT NOT NULL DEFAULT 0 CHECK (reserved_micro_usd >= 0),
    spent_micro_usd BIGINT NOT NULL DEFAULT 0 CHECK (spent_micro_usd >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, budget_root_id)
);

CREATE TABLE governance_budget_reservation (
    workspace_id UUID NOT NULL,
    reservation_id UUID NOT NULL,
    budget_root_id UUID NOT NULL,
    case_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    obligation_id UUID NOT NULL,
    resource TEXT NOT NULL CHECK (length(resource) BETWEEN 1 AND 128 AND resource = btrim(resource)),
    control_epoch BIGINT NOT NULL CHECK (control_epoch >= 0),
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    root_cap_micro_usd BIGINT NOT NULL CHECK (root_cap_micro_usd > 0),
    window_cap_micro_usd BIGINT NOT NULL CHECK (window_cap_micro_usd > 0),
    max_attempt_cost_micro_usd BIGINT NOT NULL CHECK (max_attempt_cost_micro_usd > 0),
    retry_allowance BIGINT NOT NULL CHECK (retry_allowance >= 0),
    retry_policy_bounded BOOLEAN NOT NULL CHECK (retry_policy_bounded),
    retry_allowance_remaining BIGINT NOT NULL CHECK (retry_allowance_remaining >= 0),
    attempts_started BIGINT NOT NULL DEFAULT 0 CHECK (attempts_started >= 0),
    total_cap_micro_usd BIGINT NOT NULL CHECK (total_cap_micro_usd > 0),
    remaining_micro_usd BIGINT NOT NULL CHECK (remaining_micro_usd >= 0),
    debited_micro_usd BIGINT NOT NULL DEFAULT 0 CHECK (debited_micro_usd >= 0),
    settled_micro_usd BIGINT NOT NULL DEFAULT 0 CHECK (settled_micro_usd >= 0),
    state TEXT NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved', 'settled')),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    settlement_receipt_id TEXT,
    usage_known BOOLEAN NOT NULL DEFAULT false,
    termination_known BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    settled_at TIMESTAMPTZ,
    PRIMARY KEY (workspace_id, reservation_id),
    CHECK (window_end > window_start),
    CHECK (retry_allowance_remaining <= retry_allowance),
    CHECK (attempts_started <= retry_allowance + 1),
    CHECK (settled_micro_usd <= total_cap_micro_usd),
    CHECK (
        (state = 'reserved' AND remaining_micro_usd + debited_micro_usd = total_cap_micro_usd)
        OR (state = 'settled' AND remaining_micro_usd = 0 AND debited_micro_usd = settled_micro_usd)
    ),
    CHECK ((state = 'settled') = (settlement_receipt_id IS NOT NULL)),
    CHECK ((state = 'settled') = termination_known),
    CHECK ((state = 'settled') = (settled_at IS NOT NULL))
);

CREATE TABLE governance_budget_journal (
    workspace_id UUID NOT NULL,
    reservation_id UUID NOT NULL,
    event_key TEXT NOT NULL CHECK (length(event_key) BETWEEN 1 AND 200 AND event_key = btrim(event_key)),
    event_type TEXT NOT NULL CHECK (event_type IN ('reserve', 'debit', 'settle')),
    event_digest TEXT NOT NULL CHECK (event_digest ~ '^[0-9a-f]{64}$'),
    expected_revision BIGINT NOT NULL CHECK (expected_revision >= 0),
    resulting_revision BIGINT NOT NULL CHECK (resulting_revision >= expected_revision),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, reservation_id, event_key)
);

CREATE TABLE governance_budget_outbox (
    workspace_id UUID NOT NULL,
    event_id UUID NOT NULL DEFAULT gen_random_uuid(),
    reservation_id UUID NOT NULL,
    event_key TEXT NOT NULL CHECK (length(event_key) BETWEEN 1 AND 200 AND event_key = btrim(event_key)),
    event_type TEXT NOT NULL CHECK (event_type IN ('admit', 'settled')),
    case_id UUID NOT NULL,
    attempt_id UUID NOT NULL,
    obligation_id UUID NOT NULL,
    payload JSONB NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'claimed', 'delivered')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, event_id),
    UNIQUE (workspace_id, reservation_id, event_key)
);
