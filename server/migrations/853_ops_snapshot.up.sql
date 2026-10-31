-- Ops page (CHE-2586 L1). Forward snapshots of per-machine task and agent
-- state, 5-minute rollups of those snapshots, and an append-only requeue
-- event log. No foreign keys and no cascades: a deleted runtime or task must
-- never delete history. daemon_id is TEXT; the reserved keys __cloud__,
-- __unbound__ and __unknown__ stand in for machines that have no daemon id.

CREATE TABLE IF NOT EXISTS ops_snapshot (
    workspace_id        UUID        NOT NULL,
    daemon_id           TEXT        NOT NULL,
    bucket_ts           TIMESTAMPTZ NOT NULL,
    queued INTEGER NOT NULL DEFAULT 0,
    dispatched INTEGER NOT NULL DEFAULT 0,
    waiting_dir INTEGER NOT NULL DEFAULT 0,
    running INTEGER NOT NULL DEFAULT 0,
    agents_unbound INTEGER NOT NULL DEFAULT 0,
    agents_offline INTEGER NOT NULL DEFAULT 0,
    agents_busy INTEGER NOT NULL DEFAULT 0,
    agents_waiting_dir INTEGER NOT NULL DEFAULT 0,
    agents_available INTEGER NOT NULL DEFAULT 0,
    oldest_unstarted_s  INTEGER,
    PRIMARY KEY (workspace_id, daemon_id, bucket_ts)
);

CREATE INDEX IF NOT EXISTS idx_ops_snapshot_ws_bucket ON ops_snapshot (workspace_id, bucket_ts);

CREATE TABLE IF NOT EXISTS ops_snapshot_5m (
    workspace_id        UUID        NOT NULL,
    daemon_id           TEXT        NOT NULL,
    bucket_ts           TIMESTAMPTZ NOT NULL,
    queued_max INTEGER NOT NULL DEFAULT 0,
    queued_last INTEGER NOT NULL DEFAULT 0,
    dispatched_max INTEGER NOT NULL DEFAULT 0,
    dispatched_last INTEGER NOT NULL DEFAULT 0,
    waiting_dir_max INTEGER NOT NULL DEFAULT 0,
    waiting_dir_last INTEGER NOT NULL DEFAULT 0,
    running_max INTEGER NOT NULL DEFAULT 0,
    running_last INTEGER NOT NULL DEFAULT 0,
    agents_unbound_max INTEGER NOT NULL DEFAULT 0,
    agents_unbound_last INTEGER NOT NULL DEFAULT 0,
    agents_offline_max INTEGER NOT NULL DEFAULT 0,
    agents_offline_last INTEGER NOT NULL DEFAULT 0,
    agents_busy_max INTEGER NOT NULL DEFAULT 0,
    agents_busy_last INTEGER NOT NULL DEFAULT 0,
    agents_waiting_dir_max INTEGER NOT NULL DEFAULT 0,
    agents_waiting_dir_last INTEGER NOT NULL DEFAULT 0,
    agents_available_max INTEGER NOT NULL DEFAULT 0,
    agents_available_last INTEGER NOT NULL DEFAULT 0,
    oldest_unstarted_s_max  INTEGER,
    oldest_unstarted_s_last INTEGER,
    sample_count        INTEGER     NOT NULL,
    expected_count      INTEGER     NOT NULL DEFAULT 10,
    PRIMARY KEY (workspace_id, daemon_id, bucket_ts)
);


CREATE TABLE IF NOT EXISTS agent_task_event (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL,
    task_id      UUID        NOT NULL,
    runtime_id   UUID,
    kind         TEXT        NOT NULL CHECK (kind IN ('requeue_claim_failure', 'stale_reclaim')),
    at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

