SET LOCAL lock_timeout = '2s';

-- Gated card answers (CHE-2798 B2a). answer_revision counts the changes that
-- invalidate a signed answer: reopen and class raise.
ALTER TABLE decision_request ADD COLUMN IF NOT EXISTS answer_revision INTEGER NOT NULL DEFAULT 1;

ALTER TABLE passkey_assertion_challenge DROP CONSTRAINT IF EXISTS passkey_assertion_challenge_purpose_check;
ALTER TABLE passkey_assertion_challenge ADD CONSTRAINT passkey_assertion_challenge_purpose_check
    CHECK (purpose IN ('decision_approve', 'passkey_revoke', 'gate_change', 'passkey_enroll', 'decision_answer'));
