CREATE TABLE provider_breaker (
    workspace_id uuid NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    provider text NOT NULL,
    state text NOT NULL DEFAULT 'closed' CHECK (state IN ('closed', 'open', 'half_open', 'draining')),
    opened_at timestamptz,
    next_probe_at timestamptz,
    cool_off_s integer NOT NULL DEFAULT 300 CHECK (cool_off_s > 0),
    canary_task_id uuid REFERENCES agent_task_queue(id) ON DELETE SET NULL,
    trip_evidence uuid[] NOT NULL DEFAULT '{}',
    last_5xx_at timestamptz,
    last_success_at timestamptz,
    version bigint NOT NULL DEFAULT 1,
    PRIMARY KEY (workspace_id, provider)
);
