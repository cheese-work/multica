CREATE TABLE governance_workspace_config (
    workspace_id UUID PRIMARY KEY,
    config_version BIGINT NOT NULL DEFAULT 0 CHECK (config_version >= 0),
    control_epoch BIGINT NOT NULL DEFAULT 0 CHECK (control_epoch >= 0),
    settings JSONB NOT NULL DEFAULT '{"rule_mode":"off"}'::jsonb,
    updated_by UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE governance_workspace_config_audit (
    workspace_id UUID NOT NULL,
    config_version BIGINT NOT NULL CHECK (config_version > 0),
    request_id UUID NOT NULL,
    request_digest TEXT NOT NULL CHECK (request_digest ~ '^[0-9a-f]{64}$'),
    actor_id UUID NOT NULL,
    settings_before JSONB NOT NULL,
    settings_after JSONB NOT NULL,
    control_epoch BIGINT NOT NULL CHECK (control_epoch >= 0),
    rollback_of_version BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, config_version),
    UNIQUE (workspace_id, request_id)
);

ALTER TABLE governance_case
    ADD COLUMN control_epoch BIGINT NOT NULL DEFAULT 0 CHECK (control_epoch >= 0);

ALTER TABLE governance_receipt
    ADD COLUMN control_epoch BIGINT NOT NULL DEFAULT 0 CHECK (control_epoch >= 0);
