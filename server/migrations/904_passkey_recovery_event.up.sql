SET LOCAL lock_timeout = '2s';

-- C00 passkey recovery command (CHE-2798 P6, B3): one audit action for the
-- recovery transaction.
ALTER TABLE passkey_event DROP CONSTRAINT IF EXISTS passkey_event_action_check;
ALTER TABLE passkey_event ADD CONSTRAINT passkey_event_action_check
    CHECK (action IN ('passkey_enrolled', 'passkey_revoked', 'passkey_enroll_locked', 'decision_gate_enabled', 'decision_gate_disabled', 'passkey_recovery'));
