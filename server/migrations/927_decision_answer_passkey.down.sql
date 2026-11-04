ALTER TABLE passkey_assertion_challenge DROP CONSTRAINT IF EXISTS passkey_assertion_challenge_purpose_check;
ALTER TABLE passkey_assertion_challenge ADD CONSTRAINT passkey_assertion_challenge_purpose_check
    CHECK (purpose IN ('decision_approve', 'passkey_revoke', 'gate_change', 'passkey_enroll')) NOT VALID;

ALTER TABLE decision_request DROP COLUMN IF EXISTS answer_revision;
