SET LOCAL lock_timeout = '2s';

-- B v9 S6 (CHE-5416): a decided action is revoked while it is revocable. The
-- state stays 'executed'; the revoke record is who, when, why and the passkey
-- credential that signed it. All four are set together, only on an executed row.
ALTER TABLE decision_action
    ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS revoked_by_user_id UUID,
    ADD COLUMN IF NOT EXISTS revoke_reason TEXT,
    ADD COLUMN IF NOT EXISTS revoke_webauthn_credential_id UUID,
    ADD CONSTRAINT decision_action_revoke_check CHECK (
        (revoked_at IS NULL) = (revoked_by_user_id IS NULL)
        AND (revoked_at IS NULL) = (revoke_reason IS NULL)
        AND (revoked_at IS NULL OR state = 'executed')
    );
