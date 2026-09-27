CREATE TABLE governance_jev_credential (
    workspace_id UUID PRIMARY KEY,
    envelope JSONB NOT NULL,
    updated_by UUID NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
