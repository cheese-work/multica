SET LOCAL lock_timeout = '2s';

-- Passkeys for step-up approval of decision actions (CHE-2233 plan rev-6, 4).
-- user_passkey holds the WebAuthn credential. passkey_assertion_challenge is
-- the single-use session data of one assertion; at most one is open per user
-- for every purpose (unique index in the next migration). passkey_enroll_*
-- hold the enrollment ceremony and its per-user limits; the code is stored
-- only as an HMAC. None of these rows carries a workspace.
CREATE TABLE user_passkey (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    credential_id BYTEA NOT NULL,
    public_key BYTEA NOT NULL,
    attestation_type TEXT NOT NULL DEFAULT '',
    aaguid BYTEA,
    sign_count BIGINT NOT NULL DEFAULT 0 CHECK (sign_count >= 0),
    transports TEXT[] NOT NULL DEFAULT '{}',
    flags SMALLINT NOT NULL DEFAULT 0,
    suspect_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE TABLE passkey_assertion_challenge (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    nonce BYTEA NOT NULL CHECK (octet_length(nonce) = 32),
    user_id UUID NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('decision_approve', 'passkey_revoke')),
    subject_id UUID NOT NULL,
    session_data JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    superseded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE passkey_enroll_ceremony (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,
    code_hmac BYTEA NOT NULL,
    session_data JSONB NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    consumed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE passkey_enroll_limit (
    user_id UUID PRIMARY KEY,
    hour_started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    hour_starts INTEGER NOT NULL DEFAULT 0 CHECK (hour_starts >= 0),
    day_started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    day_starts INTEGER NOT NULL DEFAULT 0 CHECK (day_starts >= 0),
    locked_until TIMESTAMPTZ
);
