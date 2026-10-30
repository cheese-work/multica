SET LOCAL lock_timeout = '2s';

-- Elevation state per task and per-operation authority (CHE-2216 plan 3.8).
-- No foreign keys (repo rule): handlers validate task, workspace and grants.
-- epoch rises by one when a fence starts; restricted_final is terminal.
-- grant_ids and scopes are frozen at the first elevated claim of the task; a
-- later grant never widens a task that already has a row.
-- launch_scopes_ended_at is written by the daemon fence slice (B4b) once the
-- processes holding a device: or docker scope have exited.
CREATE TABLE IF NOT EXISTS task_elevation (
    task_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    grant_ids uuid[] NOT NULL CHECK (cardinality(grant_ids) >= 1),
    scopes text[] NOT NULL CHECK (cardinality(scopes) BETWEEN 1 AND 16),
    epoch bigint NOT NULL DEFAULT 1,
    state text NOT NULL DEFAULT 'elevated' CHECK (state IN ('elevated', 'fencing', 'restricted_final')),
    launch_scopes_ended_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- ponytail: no secondary indexes. Both tables stay small (a person grants each
-- elevation); add a CONCURRENTLY index in its own migration if a scan shows up.
-- One authorized elevated operation. input_hash binds it to the exact input the
-- actor hashed before executing. lease_until is the 10-minute operation deadline.
CREATE TABLE IF NOT EXISTS elevated_op (
    op_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    task_id uuid NOT NULL,
    epoch bigint NOT NULL,
    scope text NOT NULL,
    input_hash text NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
    state text NOT NULL DEFAULT 'authorized' CHECK (state IN ('authorized', 'completed', 'failed', 'aborted', 'unknown')),
    lease_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
