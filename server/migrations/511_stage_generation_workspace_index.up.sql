-- Supports the workspace-teardown delete (DeleteWorkspaceLeafData) added for
-- stage_generation alongside issue_reaction and similar workspace-scoped
-- leaf tables. Its own single-statement migration: CREATE INDEX CONCURRENTLY
-- cannot run inside a transaction or alongside other statements.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_stage_generation_workspace_id
    ON stage_generation (workspace_id);
