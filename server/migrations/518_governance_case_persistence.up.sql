-- CHE-704 / C01: durable, default-off MJ case evidence. These rows only
-- persist lifecycle/evidence state; no migration, trigger, or query here can
-- admit work, call a provider, or apply a correction.
--
-- No foreign keys or cascades: relationships are checked by later lifecycle
-- services and workspace teardown explicitly removes every table below.
CREATE TABLE IF NOT EXISTS governance_case (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    subject_type        TEXT NOT NULL,
    subject_id          UUID NOT NULL,
    subject_revision    BIGINT NOT NULL,
    rule_id             UUID NOT NULL,
    generation          INTEGER NOT NULL CHECK (generation >= 0),
    material_fingerprint TEXT NOT NULL,
    state               TEXT NOT NULL CHECK (state IN (
        'captured', 'evidence_ready', 'jev_evaluating', 'agent_escalation',
        'agent_attempt', 'next_attempt', 'correction_pending', 'executing',
        'refreshing', 'human_review', 'abstained', 'resolved', 'dismissed',
        'invalidated', 'failed', 'parked'
    )),
    state_revision      BIGINT NOT NULL DEFAULT 0 CHECK (state_revision >= 0),
    authority_lineage   JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(authority_lineage) = 'array'),
    trigger_aliases     JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(trigger_aliases) = 'array'),
    evidence_id         UUID,
    evidence_digest     TEXT NOT NULL DEFAULT '',
    rule_revision       TEXT NOT NULL DEFAULT '',
    activation_revision TEXT NOT NULL DEFAULT '',
    config_revision     TEXT NOT NULL DEFAULT '',
    lease_token         UUID,
    lease_expires_at    TIMESTAMPTZ,
    current_attempt_id  UUID,
    current_action_id   UUID,
    predecessor_case_id UUID,
    budget_root_id      UUID NOT NULL,
    evidence_epoch      INTEGER NOT NULL DEFAULT 0 CHECK (evidence_epoch >= 0),
    refresh_count       INTEGER NOT NULL DEFAULT 0 CHECK (refresh_count >= 0),
    absolute_deadline   TIMESTAMPTZ,
    frozen_strategy     JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(frozen_strategy) = 'array'),
    reason              TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS governance_case_transition (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id             UUID NOT NULL,
    case_id                  UUID NOT NULL,
    resulting_state_revision BIGINT NOT NULL CHECK (resulting_state_revision >= 0),
    expected_state_revision  BIGINT NOT NULL CHECK (expected_state_revision >= 0),
    from_state               TEXT NOT NULL,
    to_state                 TEXT NOT NULL,
    cause_event_key          TEXT NOT NULL,
    actor_type               TEXT NOT NULL CHECK (actor_type IN ('member', 'agent', 'system')),
    actor_id                 UUID,
    sanitized_reason         TEXT NOT NULL DEFAULT '',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS governance_attempt (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id             UUID NOT NULL,
    case_id                  UUID NOT NULL,
    ordinal                  INTEGER NOT NULL CHECK (ordinal >= 0),
    kind                     TEXT NOT NULL CHECK (kind IN ('jev', 'agent')),
    candidate_id             UUID,
    task_id                  UUID,
    obligation_id            UUID,
    input_digest             TEXT NOT NULL,
    attempt_fence            UUID NOT NULL,
    claimed_at               TIMESTAMPTZ,
    deadline_at              TIMESTAMPTZ,
    terminal_reason          TEXT NOT NULL DEFAULT '',
    terminal_at              TIMESTAMPTZ,
    confidence               JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(confidence) = 'object'),
    result                   JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(result) = 'object'),
    usage                    JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(usage) = 'object'),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (terminal_at IS NULL OR terminal_reason <> '')
);

CREATE TABLE IF NOT EXISTS governance_evaluation (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id            UUID NOT NULL,
    case_id                 UUID NOT NULL,
    attempt_id              UUID,
    trigger_identity        TEXT NOT NULL,
    subject_revision_vector JSONB NOT NULL CHECK (jsonb_typeof(subject_revision_vector) = 'object'),
    snapshot                JSONB NOT NULL CHECK (jsonb_typeof(snapshot) = 'object'),
    snapshot_digest         TEXT NOT NULL,
    snapshot_schema_version SMALLINT NOT NULL CHECK (snapshot_schema_version > 0),
    required_complete       BOOLEAN NOT NULL,
    candidate_map           JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (
        jsonb_typeof(candidate_map) = 'array' AND jsonb_array_length(candidate_map) <= 16
    ),
    citation_map            JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (
        jsonb_typeof(citation_map) = 'array' AND jsonb_array_length(citation_map) <= 16
    ),
    estimated_tokens        INTEGER NOT NULL CHECK (estimated_tokens >= 0 AND estimated_tokens <= 8000),
    applicable_rule_digests JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(applicable_rule_digests) = 'array'),
    question_criteria_hash  TEXT NOT NULL,
    requested_model         TEXT NOT NULL DEFAULT '',
    returned_model          TEXT NOT NULL DEFAULT '',
    answers                 JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(answers) = 'array'),
    captured_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS governance_evaluation_source (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL,
    evaluation_id       UUID NOT NULL,
    object_type         TEXT NOT NULL,
    object_id           TEXT NOT NULL,
    object_revision     TEXT NOT NULL,
    object_digest       TEXT NOT NULL,
    copied_context      JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(copied_context) = 'object'),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
