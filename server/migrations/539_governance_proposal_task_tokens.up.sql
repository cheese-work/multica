ALTER TABLE task_token
    ADD COLUMN purpose TEXT NOT NULL DEFAULT 'agent_task'
        CHECK (purpose IN ('agent_task', 'governance_proposal')),
    ADD COLUMN governance_case_id UUID,
    ADD COLUMN governance_attempt_id UUID,
    ADD COLUMN governance_attempt_fence UUID,
    ADD COLUMN governance_evidence_epoch INTEGER,
    ADD CONSTRAINT task_token_governance_proposal_binding_check CHECK (
        (purpose = 'agent_task'
            AND governance_case_id IS NULL
            AND governance_attempt_id IS NULL
            AND governance_attempt_fence IS NULL
            AND governance_evidence_epoch IS NULL)
        OR
        (purpose = 'governance_proposal'
            AND governance_case_id IS NOT NULL
            AND governance_attempt_id IS NOT NULL
            AND governance_attempt_fence IS NOT NULL
            AND governance_evidence_epoch IS NOT NULL
            AND governance_evidence_epoch >= 0)
    );

ALTER TABLE governance_attempt
    ADD COLUMN last_heartbeat_at TIMESTAMPTZ;
