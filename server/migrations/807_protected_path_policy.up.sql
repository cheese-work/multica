SET LOCAL lock_timeout = '2s';

-- Workspace protected-path policy (CHE-2216 plan 3.2): the push broker refuses a
-- change to any matching path. Server-side and admin-written, never a file in
-- the pushed repository. No foreign keys (repo rule): the handler validates the
-- workspace and member. A grant scope `protected_paths:<name>` names one row.
CREATE TABLE IF NOT EXISTS protected_path_policy (
    workspace_id uuid NOT NULL,
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    patterns text[] NOT NULL CHECK (cardinality(patterns) BETWEEN 1 AND 64),
    updated_by_member_id uuid NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (workspace_id, name)
);
