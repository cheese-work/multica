-- Passkey enrollment and revocation audit. User-scoped: a passkey belongs to a
-- person, not a workspace, so activity_log (workspace_id NOT NULL) cannot hold it.
CREATE TABLE passkey_event (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('passkey_enrolled', 'passkey_revoked', 'passkey_enroll_locked')),
    details JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
