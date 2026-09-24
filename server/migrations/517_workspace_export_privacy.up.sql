-- CHE-766 export privacy controls for the CHE-755 provenance-export capability.
--
-- export_redaction_mode selects how CHE-755 export content is scrubbed before
-- it leaves the server. 'small' is the only implemented mode today: it keeps
-- the existing unconditional redact.Text secret/credential masking and
-- nothing more. 'strict' names Cheese's approved roadmap mode (broader PII
-- redaction) but has no implementation yet; the export handler fails closed
-- on any value it does not recognize as fully implemented, so a workspace
-- can never select a mode that silently does less than advertised.
--
-- export_manifest_retention_days bounds how long provenance_export_log rows
-- (identifiers/digests/exclusion reasons only, never exported content, see
-- migration 515) are kept before scheduled deletion. Default 90 matches the
-- product default; a workspace admin can change it from the dashboard once
-- built.
--
-- Both are plain typed columns, not the generic `workspace.settings` JSONB
-- blob: that blob is overwritten wholesale by UpdateWorkspace with no
-- server-side schema validation, which is the wrong place for a
-- security-relevant, fail-closed policy value.
ALTER TABLE workspace
    ADD COLUMN export_redaction_mode TEXT NOT NULL DEFAULT 'small',
    ADD COLUMN export_manifest_retention_days INTEGER NOT NULL DEFAULT 90;

ALTER TABLE workspace
    ADD CONSTRAINT workspace_export_redaction_mode_check
        CHECK (export_redaction_mode IN ('small', 'strict')),
    ADD CONSTRAINT workspace_export_manifest_retention_days_check
        CHECK (export_manifest_retention_days > 0 AND export_manifest_retention_days <= 3650);

COMMENT ON COLUMN workspace.export_redaction_mode IS
    'CHE-766: redaction mode for CHE-755 provenance export. Only "small" is implemented and enforced server-side; "strict" is stored for forward compatibility but is rejected fail-closed by the export handler until it ships.';
COMMENT ON COLUMN workspace.export_manifest_retention_days IS
    'CHE-766: days a provenance_export_log row is kept before scheduled deletion. Default 90.';
