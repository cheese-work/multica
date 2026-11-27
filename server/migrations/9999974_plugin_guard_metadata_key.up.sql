-- Metadata keys a guard hook declares in guard.metadata_writers. The consented
-- manifest stays the source of truth; this table is the lookup the issue
-- metadata write path reads to decide whether a write needs provenance.
--
-- Relationships are application-owned by repository policy: install, upgrade,
-- enable/disable, uninstall and workspace teardown reconcile these rows in the
-- same transaction as plugin_installation. No foreign key or cascade is used.
-- enabled mirrors plugin_installation.enabled, so a disabled installation
-- declares nothing.
CREATE TABLE plugin_guard_metadata_key (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    metadata_key TEXT NOT NULL CHECK (char_length(metadata_key) BETWEEN 1 AND 64),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
