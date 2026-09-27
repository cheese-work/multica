CREATE TABLE governance_workspace_credential (
    workspace_id UUID NOT NULL,
    purpose TEXT NOT NULL,
    record_id UUID NOT NULL,
    key_id TEXT NOT NULL,
    algorithm TEXT NOT NULL,
    nonce BYTEA NOT NULL,
    ciphertext BYTEA NOT NULL,
    rotation_version BIGINT NOT NULL DEFAULT 1 CHECK (rotation_version >= 1),
    created_by UUID NOT NULL,
    updated_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, purpose)
);

CREATE TABLE governance_workspace_credential_audit (
    workspace_id UUID NOT NULL,
    purpose TEXT NOT NULL,
    record_id UUID NOT NULL,
    action TEXT NOT NULL CHECK (action IN ('write', 'rotate', 'delete')),
    rotation_version BIGINT NOT NULL CHECK (rotation_version >= 1),
    key_id TEXT NOT NULL,
    actor_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
