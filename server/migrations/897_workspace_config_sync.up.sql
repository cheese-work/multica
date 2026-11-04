SET LOCAL lock_timeout = '2s';

-- Owner-only config-sync enrollment (CHE-3284): one JSON settings value per
-- workspace, written only through the owner digest CAS. `id` mirrors
-- workspace_id so the CAS statement's `WHERE id = $ AND workspace_id = $`
-- shape works unchanged. Empty settings = not enrolled. No foreign keys
-- (repo rule); workspace teardown deletes the row.
CREATE TABLE IF NOT EXISTS workspace_config_sync (
    workspace_id uuid PRIMARY KEY,
    id uuid GENERATED ALWAYS AS (workspace_id) STORED,
    settings text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
