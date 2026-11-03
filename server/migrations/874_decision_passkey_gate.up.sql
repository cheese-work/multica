SET LOCAL lock_timeout = '2s';

-- Decision passkey gate (CHE-2798 P6, B1b). The workspace setting starts off
-- everywhere; policy_revision counts its changes. passkey_state is per user:
-- recovery_required is set only by the C00 recovery command (B3).
ALTER TABLE workspace
    ADD COLUMN IF NOT EXISTS decision_passkey_gate TEXT NOT NULL DEFAULT 'off' CHECK (decision_passkey_gate IN ('off', 'block')),
    ADD COLUMN IF NOT EXISTS policy_revision INTEGER NOT NULL DEFAULT 1;

ALTER TABLE "user"
    ADD COLUMN IF NOT EXISTS passkey_state TEXT NOT NULL DEFAULT 'ready' CHECK (passkey_state IN ('ready', 'recovery_required'));

-- New assertion purposes: gate_change signs a gate enable/disable;
-- passkey_enroll signs the enrollment of another passkey while the gate is on.
ALTER TABLE passkey_assertion_challenge DROP CONSTRAINT IF EXISTS passkey_assertion_challenge_purpose_check;
ALTER TABLE passkey_assertion_challenge ADD CONSTRAINT passkey_assertion_challenge_purpose_check
    CHECK (purpose IN ('decision_approve', 'passkey_revoke', 'gate_change', 'passkey_enroll'));

ALTER TABLE passkey_event DROP CONSTRAINT IF EXISTS passkey_event_action_check;
ALTER TABLE passkey_event ADD CONSTRAINT passkey_event_action_check
    CHECK (action IN ('passkey_enrolled', 'passkey_revoked', 'passkey_enroll_locked', 'decision_gate_enabled', 'decision_gate_disabled'));
