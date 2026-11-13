SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_action
    DROP CONSTRAINT IF EXISTS decision_action_revoke_check,
    DROP COLUMN IF EXISTS revoke_webauthn_credential_id,
    DROP COLUMN IF EXISTS revoke_reason,
    DROP COLUMN IF EXISTS revoked_by_user_id,
    DROP COLUMN IF EXISTS revoked_at;
