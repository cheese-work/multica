-- CHE-755 provenance export audit log. One row per successful
-- `multica provenance export` response, written synchronously before the
-- response is sent: if this insert fails the export fails closed and no
-- records leave the server.
--
-- manifest holds only identifiers, revisions, digests and exclusion reasons —
-- never titles, descriptions or comment bodies — so the audit trail itself
-- cannot become a second copy of the exported content.
--
-- No foreign keys per repo convention (see AGENTS.md). Workspace teardown
-- deletes these rows in DeleteWorkspaceLeafData.
CREATE TABLE IF NOT EXISTS provenance_export_log (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL,
    actor_type      TEXT NOT NULL,
    actor_id        UUID NOT NULL,
    request_digest  TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    cutoff          TIMESTAMPTZ NOT NULL,
    source_count    INTEGER NOT NULL,
    included_count  INTEGER NOT NULL,
    excluded_count  INTEGER NOT NULL,
    manifest        JSONB NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
