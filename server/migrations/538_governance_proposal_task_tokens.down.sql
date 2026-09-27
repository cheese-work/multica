ALTER TABLE governance_attempt
    DROP COLUMN IF EXISTS last_heartbeat_at;

ALTER TABLE task_token
    DROP CONSTRAINT IF EXISTS task_token_governance_proposal_binding_check,
    DROP COLUMN IF EXISTS governance_evidence_epoch,
    DROP COLUMN IF EXISTS governance_attempt_fence,
    DROP COLUMN IF EXISTS governance_attempt_id,
    DROP COLUMN IF EXISTS governance_case_id,
    DROP COLUMN IF EXISTS purpose;
