ALTER TABLE passkey_event DROP CONSTRAINT IF EXISTS passkey_event_action_check;
ALTER TABLE passkey_event ADD CONSTRAINT passkey_event_action_check
    CHECK (action IN ('passkey_enrolled', 'passkey_revoked', 'passkey_enroll_locked')) NOT VALID;

ALTER TABLE passkey_assertion_challenge DROP CONSTRAINT IF EXISTS passkey_assertion_challenge_purpose_check;
ALTER TABLE passkey_assertion_challenge ADD CONSTRAINT passkey_assertion_challenge_purpose_check
    CHECK (purpose IN ('decision_approve', 'passkey_revoke')) NOT VALID;

ALTER TABLE "user" DROP COLUMN IF EXISTS passkey_state;
ALTER TABLE workspace DROP COLUMN IF EXISTS policy_revision, DROP COLUMN IF EXISTS decision_passkey_gate;
