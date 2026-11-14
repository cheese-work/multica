SET LOCAL lock_timeout = '2s';

-- The blind label queue a labeller task was served (CHE-4383 B4b). Ids and seed
-- are stored when the queue is served, so the denominator is reproducible and a
-- task that asks again gets the same sample. No foreign key: workspace_id,
-- task_id and agent_id are checked in application code.
CREATE TABLE system1_label_sample (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    scope text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('burn_in', 'eval')),
    task_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    seed bigint NOT NULL,
    observation_ids uuid[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (task_id, scope, kind)
);
