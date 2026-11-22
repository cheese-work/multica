SET LOCAL lock_timeout = '2s';

-- Person-signed edit of the workspace guard policy (CHE-5207): a new assertion
-- purpose, bound to the workspace, the policy before and the policy after.
ALTER TABLE passkey_assertion_challenge DROP CONSTRAINT IF EXISTS passkey_assertion_challenge_purpose_check;
ALTER TABLE passkey_assertion_challenge ADD CONSTRAINT passkey_assertion_challenge_purpose_check
    CHECK (purpose IN ('decision_approve', 'passkey_revoke', 'gate_change', 'passkey_enroll', 'decision_answer', 'guard_policy_change'));
