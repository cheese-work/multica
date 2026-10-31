-- Ops page (CHE-2586 L1). Sampler, rollup, retention and chart queries.
-- Machine key = agent_runtime.daemon_id; NULL/empty -> '__cloud__'; agents
-- with no runtime -> '__unbound__'; terminal tasks with no runtime ->
-- '__unknown__'. Task status 'deferred' is never counted.

-- name: InsertOpsSnapshot :execrows
-- One statement, one MVCC snapshot: every count below is read from the same
-- instant, so per-tick sums over machine keys equal workspace totals.
-- An agent's state, in order: unbound, offline, busy (dispatched/running
-- task), waiting_dir, available. ON CONFLICT DO NOTHING drops the duplicate
-- from an overlapping server boot.
WITH machine AS (
    SELECT r.id AS runtime_id, r.workspace_id, r.status,
           CASE WHEN r.daemon_id IS NULL OR r.daemon_id = '' THEN '__cloud__' ELSE r.daemon_id END AS key
    FROM agent_runtime r
),
active AS (
    SELECT t.agent_id, t.runtime_id, t.status, t.created_at
    FROM agent_task_queue t
    WHERE t.status IN ('queued', 'dispatched', 'waiting_local_directory', 'running')
),
agent_busy AS (
    SELECT agent_id,
           bool_or(status IN ('dispatched', 'running')) AS busy,
           bool_or(status = 'waiting_local_directory') AS waiting
    FROM active GROUP BY agent_id
),
agent_state AS (
    SELECT a.workspace_id,
           CASE WHEN m.runtime_id IS NULL THEN '__unbound__' ELSE m.key END AS key,
           CASE WHEN m.runtime_id IS NULL THEN 'unbound'
                WHEN m.status = 'offline' THEN 'offline'
                WHEN COALESCE(b.busy, false) THEN 'busy'
                WHEN COALESCE(b.waiting, false) THEN 'waiting_dir'
                ELSE 'available' END AS state
    FROM agent a
    LEFT JOIN machine m ON m.runtime_id = a.runtime_id
    LEFT JOIN agent_busy b ON b.agent_id = a.id
    WHERE a.archived_at IS NULL
),
agent_counts AS (
    SELECT workspace_id, key,
           count(*) FILTER (WHERE state = 'unbound')::int AS agents_unbound,
           count(*) FILTER (WHERE state = 'offline')::int AS agents_offline,
           count(*) FILTER (WHERE state = 'busy')::int AS agents_busy,
           count(*) FILTER (WHERE state = 'waiting_dir')::int AS agents_waiting_dir,
           count(*) FILTER (WHERE state = 'available')::int AS agents_available
    FROM agent_state GROUP BY workspace_id, key
),
task_counts AS (
    SELECT m.workspace_id, m.key,
           count(*) FILTER (WHERE t.status = 'queued')::int AS queued,
           count(*) FILTER (WHERE t.status = 'dispatched')::int AS dispatched,
           count(*) FILTER (WHERE t.status = 'waiting_local_directory')::int AS waiting_dir,
           count(*) FILTER (WHERE t.status = 'running')::int AS running,
           min(t.created_at) FILTER (WHERE t.status IN ('queued', 'dispatched', 'waiting_local_directory')) AS oldest
    FROM active t JOIN machine m ON m.runtime_id = t.runtime_id
    GROUP BY m.workspace_id, m.key
),
keys AS (
    SELECT workspace_id, key FROM machine
    UNION SELECT workspace_id, key FROM agent_counts
)
INSERT INTO ops_snapshot (workspace_id, daemon_id, bucket_ts, queued, dispatched, waiting_dir, running,
                          agents_unbound, agents_offline, agents_busy, agents_waiting_dir, agents_available,
                          oldest_unstarted_s)
SELECT k.workspace_id, k.key, @bucket_ts::timestamptz,
       COALESCE(tc.queued, 0), COALESCE(tc.dispatched, 0), COALESCE(tc.waiting_dir, 0), COALESCE(tc.running, 0),
       COALESCE(ac.agents_unbound, 0), COALESCE(ac.agents_offline, 0), COALESCE(ac.agents_busy, 0),
       COALESCE(ac.agents_waiting_dir, 0), COALESCE(ac.agents_available, 0),
       GREATEST(0, extract(epoch FROM sqlc.arg(bucket_ts)::timestamptz - tc.oldest))::int
FROM keys k
LEFT JOIN task_counts tc ON tc.workspace_id = k.workspace_id AND tc.key = k.key
LEFT JOIN agent_counts ac ON ac.workspace_id = k.workspace_id AND ac.key = k.key
ON CONFLICT (workspace_id, daemon_id, bucket_ts) DO NOTHING;

-- name: RollupOpsSnapshot5m :execrows
-- Folds raw rows in [from_ts, to_ts) into 5-minute intervals. Idempotent:
-- a rerun recomputes from the raw rows still present. Intervals with no raw
-- row are not written (gap, not zero).
INSERT INTO ops_snapshot_5m (workspace_id, daemon_id, bucket_ts,
    queued_max, queued_last, dispatched_max, dispatched_last, waiting_dir_max, waiting_dir_last, running_max, running_last, agents_unbound_max, agents_unbound_last, agents_offline_max, agents_offline_last, agents_busy_max, agents_busy_last, agents_waiting_dir_max, agents_waiting_dir_last, agents_available_max, agents_available_last, oldest_unstarted_s_max, oldest_unstarted_s_last,
    sample_count, expected_count)
SELECT workspace_id, daemon_id, date_bin('5 minutes', bucket_ts, TIMESTAMPTZ 'epoch') AS interval_ts,
    max(queued), (array_agg(queued ORDER BY bucket_ts DESC))[1], max(dispatched), (array_agg(dispatched ORDER BY bucket_ts DESC))[1], max(waiting_dir), (array_agg(waiting_dir ORDER BY bucket_ts DESC))[1], max(running), (array_agg(running ORDER BY bucket_ts DESC))[1], max(agents_unbound), (array_agg(agents_unbound ORDER BY bucket_ts DESC))[1], max(agents_offline), (array_agg(agents_offline ORDER BY bucket_ts DESC))[1], max(agents_busy), (array_agg(agents_busy ORDER BY bucket_ts DESC))[1], max(agents_waiting_dir), (array_agg(agents_waiting_dir ORDER BY bucket_ts DESC))[1], max(agents_available), (array_agg(agents_available ORDER BY bucket_ts DESC))[1], max(oldest_unstarted_s), (array_agg(oldest_unstarted_s ORDER BY bucket_ts DESC))[1],
    count(*)::int, 10
FROM ops_snapshot
WHERE bucket_ts >= @from_ts::timestamptz AND bucket_ts < @to_ts::timestamptz
GROUP BY workspace_id, daemon_id, interval_ts
ON CONFLICT (workspace_id, daemon_id, bucket_ts) DO UPDATE SET
    queued_max = EXCLUDED.queued_max,
    queued_last = EXCLUDED.queued_last,
    dispatched_max = EXCLUDED.dispatched_max,
    dispatched_last = EXCLUDED.dispatched_last,
    waiting_dir_max = EXCLUDED.waiting_dir_max,
    waiting_dir_last = EXCLUDED.waiting_dir_last,
    running_max = EXCLUDED.running_max,
    running_last = EXCLUDED.running_last,
    agents_unbound_max = EXCLUDED.agents_unbound_max,
    agents_unbound_last = EXCLUDED.agents_unbound_last,
    agents_offline_max = EXCLUDED.agents_offline_max,
    agents_offline_last = EXCLUDED.agents_offline_last,
    agents_busy_max = EXCLUDED.agents_busy_max,
    agents_busy_last = EXCLUDED.agents_busy_last,
    agents_waiting_dir_max = EXCLUDED.agents_waiting_dir_max,
    agents_waiting_dir_last = EXCLUDED.agents_waiting_dir_last,
    agents_available_max = EXCLUDED.agents_available_max,
    agents_available_last = EXCLUDED.agents_available_last,
    oldest_unstarted_s_max = EXCLUDED.oldest_unstarted_s_max,
    oldest_unstarted_s_last = EXCLUDED.oldest_unstarted_s_last,
    sample_count = EXCLUDED.sample_count;

-- name: DeleteOpsSnapshotBefore :execrows
DELETE FROM ops_snapshot WHERE bucket_ts < @cutoff::timestamptz;

-- name: DeleteOpsSnapshot5mBefore :execrows
DELETE FROM ops_snapshot_5m WHERE bucket_ts < @cutoff::timestamptz;

-- name: DeleteAgentTaskEventBefore :execrows
DELETE FROM agent_task_event WHERE at < @cutoff::timestamptz;

-- name: DeleteOpsSamplerCronExecutionsBefore :execrows
-- The 30 s sampler writes 2880 lease rows a day; sys_cron_executions has no
-- other retention, so this job trims its own.
DELETE FROM sys_cron_executions WHERE job_name = 'ops_snapshot_sampler' AND plan_time < @cutoff::timestamptz;

-- name: ListOpsSnapshotSeries :many
-- Charts 1 and 4 for windows up to 7 d: raw 30 s rows binned to @bucket_s.
-- `_max` feeds lines, `_last` feeds point-in-time stacks.
SELECT date_bin(make_interval(secs => @bucket_s::int), bucket_ts, TIMESTAMPTZ 'epoch') AS ts,
       daemon_id,
       max(queued)::int AS queued_max,
       (array_agg(queued ORDER BY bucket_ts DESC))[1]::int AS queued_last,
       max(dispatched)::int AS dispatched_max,
       (array_agg(dispatched ORDER BY bucket_ts DESC))[1]::int AS dispatched_last,
       max(waiting_dir)::int AS waiting_dir_max,
       (array_agg(waiting_dir ORDER BY bucket_ts DESC))[1]::int AS waiting_dir_last,
       max(running)::int AS running_max,
       (array_agg(running ORDER BY bucket_ts DESC))[1]::int AS running_last,
       max(agents_unbound)::int AS agents_unbound_max,
       (array_agg(agents_unbound ORDER BY bucket_ts DESC))[1]::int AS agents_unbound_last,
       max(agents_offline)::int AS agents_offline_max,
       (array_agg(agents_offline ORDER BY bucket_ts DESC))[1]::int AS agents_offline_last,
       max(agents_busy)::int AS agents_busy_max,
       (array_agg(agents_busy ORDER BY bucket_ts DESC))[1]::int AS agents_busy_last,
       max(agents_waiting_dir)::int AS agents_waiting_dir_max,
       (array_agg(agents_waiting_dir ORDER BY bucket_ts DESC))[1]::int AS agents_waiting_dir_last,
       max(agents_available)::int AS agents_available_max,
       (array_agg(agents_available ORDER BY bucket_ts DESC))[1]::int AS agents_available_last,
       max(oldest_unstarted_s)::int AS oldest_unstarted_s_max,
       (array_agg(oldest_unstarted_s ORDER BY bucket_ts DESC))[1]::int AS oldest_unstarted_s_last,
       count(*)::int AS sample_count
FROM ops_snapshot
WHERE workspace_id = @workspace_id AND bucket_ts >= @since::timestamptz AND bucket_ts < @until::timestamptz
GROUP BY ts, daemon_id
ORDER BY ts, daemon_id;

-- name: ListOpsSnapshot5mSeries :many
-- Same shape for the 30 d window, from the 5-minute rollups.
SELECT date_bin(make_interval(secs => @bucket_s::int), bucket_ts, TIMESTAMPTZ 'epoch') AS ts,
       daemon_id,
       max(queued_max)::int AS queued_max,
       (array_agg(queued_last ORDER BY bucket_ts DESC))[1]::int AS queued_last,
       max(dispatched_max)::int AS dispatched_max,
       (array_agg(dispatched_last ORDER BY bucket_ts DESC))[1]::int AS dispatched_last,
       max(waiting_dir_max)::int AS waiting_dir_max,
       (array_agg(waiting_dir_last ORDER BY bucket_ts DESC))[1]::int AS waiting_dir_last,
       max(running_max)::int AS running_max,
       (array_agg(running_last ORDER BY bucket_ts DESC))[1]::int AS running_last,
       max(agents_unbound_max)::int AS agents_unbound_max,
       (array_agg(agents_unbound_last ORDER BY bucket_ts DESC))[1]::int AS agents_unbound_last,
       max(agents_offline_max)::int AS agents_offline_max,
       (array_agg(agents_offline_last ORDER BY bucket_ts DESC))[1]::int AS agents_offline_last,
       max(agents_busy_max)::int AS agents_busy_max,
       (array_agg(agents_busy_last ORDER BY bucket_ts DESC))[1]::int AS agents_busy_last,
       max(agents_waiting_dir_max)::int AS agents_waiting_dir_max,
       (array_agg(agents_waiting_dir_last ORDER BY bucket_ts DESC))[1]::int AS agents_waiting_dir_last,
       max(agents_available_max)::int AS agents_available_max,
       (array_agg(agents_available_last ORDER BY bucket_ts DESC))[1]::int AS agents_available_last,
       max(oldest_unstarted_s_max)::int AS oldest_unstarted_s_max,
       (array_agg(oldest_unstarted_s_last ORDER BY bucket_ts DESC))[1]::int AS oldest_unstarted_s_last,
       sum(sample_count)::int AS sample_count
FROM ops_snapshot_5m
WHERE workspace_id = @workspace_id AND bucket_ts >= @since::timestamptz AND bucket_ts < @until::timestamptz
GROUP BY ts, daemon_id
ORDER BY ts, daemon_id;

-- name: GetOpsSnapshotBounds :one
-- First and last snapshot time for the workspace; rollups extend the first.
SELECT LEAST((SELECT min(bucket_ts) FROM ops_snapshot WHERE ops_snapshot.workspace_id = @workspace_id),
             (SELECT min(bucket_ts) FROM ops_snapshot_5m WHERE ops_snapshot_5m.workspace_id = @workspace_id))::timestamptz AS first_ts,
       (SELECT max(bucket_ts) FROM ops_snapshot WHERE ops_snapshot.workspace_id = @workspace_id)::timestamptz AS last_ts;

-- name: ListOpsSnapshotLatest :many
-- The newest tick's rows, for the KPI row.
SELECT daemon_id, bucket_ts, queued, dispatched, waiting_dir, running,
       agents_unbound, agents_offline, agents_busy, agents_waiting_dir, agents_available,
       oldest_unstarted_s
FROM ops_snapshot
WHERE ops_snapshot.workspace_id = @workspace_id
  AND bucket_ts = (SELECT max(o.bucket_ts) FROM ops_snapshot o WHERE o.workspace_id = @workspace_id)
ORDER BY daemon_id;

-- name: ListOpsRunsHistory :many
-- Chart 1 before snapshots exist (and for the whole 30 d window, which the
-- snapshot rollups cannot cover before deploy): task rows replayed at each
-- bucket boundary. `running` is exact. `queued` is approximate: a requeue
-- overwrites dispatched_at, so the queued/dispatched split is lost; the span
-- is created_at .. COALESCE(started_at, completed_at). Deferred tasks are
-- not counted.
WITH bucket AS (
    SELECT generate_series(
        date_bin(make_interval(secs => @bucket_s::int), @since::timestamptz, TIMESTAMPTZ 'epoch'),
        @until::timestamptz,
        make_interval(secs => @bucket_s::int)) AS ts
),
task AS (
    SELECT t.id, t.created_at, t.started_at, t.completed_at,
           CASE WHEN r.id IS NULL THEN '__unknown__'
                WHEN r.daemon_id IS NULL OR r.daemon_id = '' THEN '__cloud__'
                ELSE r.daemon_id END AS machine
    FROM agent_task_queue t
    JOIN agent a ON a.id = t.agent_id
    LEFT JOIN agent_runtime r ON r.id = t.runtime_id
    WHERE a.workspace_id = @workspace_id
      AND t.status <> 'deferred'
      AND t.created_at < @until::timestamptz
      AND (t.completed_at IS NULL OR t.completed_at > @since::timestamptz)
)
SELECT b.ts::timestamptz AS ts, t.machine,
       count(*) FILTER (WHERE t.started_at <= b.ts AND (t.completed_at IS NULL OR t.completed_at > b.ts))::int AS running,
       count(*) FILTER (WHERE t.created_at <= b.ts AND COALESCE(t.started_at, t.completed_at, 'infinity'::timestamptz) > b.ts)::int AS queued_approx
FROM bucket b
JOIN task t ON t.created_at <= b.ts AND COALESCE(t.completed_at, 'infinity'::timestamptz) > b.ts
GROUP BY b.ts, t.machine
ORDER BY b.ts, t.machine;

-- name: ListOpsRequeues :many
SELECT date_bin(make_interval(secs => @bucket_s::int), at, TIMESTAMPTZ 'epoch')::timestamptz AS ts,
       count(*) FILTER (WHERE kind = 'requeue_claim_failure')::int AS claim_failures,
       count(*) FILTER (WHERE kind = 'stale_reclaim')::int AS stale_reclaims
FROM agent_task_event
WHERE workspace_id = @workspace_id AND at >= @since::timestamptz AND at < @until::timestamptz
GROUP BY ts
ORDER BY ts;

-- name: ListOpsQueueWait :many
-- Chart 2. One sample per task (started_at - created_at), bucketed by
-- started_at. machine = '' is the all-machines row. Tasks that waited on an
-- escalation timer (fire_at set) are excluded: their wait is by design.
SELECT s.ts::timestamptz AS ts,
       COALESCE(s.machine, '')::text AS machine,
       count(*)::int AS samples,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY s.wait_s)::float8 AS p50_s,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY s.wait_s)::float8 AS p95_s
FROM (
    SELECT date_bin(make_interval(secs => @bucket_s::int), t.started_at, TIMESTAMPTZ 'epoch') AS ts,
           CASE WHEN r.id IS NULL THEN '__unknown__'
                WHEN r.daemon_id IS NULL OR r.daemon_id = '' THEN '__cloud__'
                ELSE r.daemon_id END AS machine,
           extract(epoch FROM t.started_at - t.created_at) AS wait_s
    FROM agent_task_queue t
    JOIN agent a ON a.id = t.agent_id
    LEFT JOIN agent_runtime r ON r.id = t.runtime_id
    WHERE a.workspace_id = @workspace_id
      AND t.started_at >= @since::timestamptz AND t.started_at < @until::timestamptz
      AND t.fire_at IS NULL
) s
GROUP BY GROUPING SETS ((s.ts, s.machine), (s.ts))
ORDER BY s.ts, machine;

-- name: GetOpsQueueWaitRecent :one
-- p95 queue wait of tasks started in the last 15 minutes (KPI).
SELECT COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM t.started_at - t.created_at)), 0)::float8 AS p95_s,
       count(*)::int AS samples
FROM agent_task_queue t
JOIN agent a ON a.id = t.agent_id
WHERE a.workspace_id = @workspace_id AND t.started_at >= @since::timestamptz AND t.fire_at IS NULL;

-- name: ListOpsFailures :many
-- Chart 3. Same population and reason rules as ListDashboardFailuresDaily:
-- every terminal completed/failed task, failure_reason '' for success,
-- 'unclassified' for a failed row with no reason.
SELECT date_bin(make_interval(secs => @bucket_s::int), t.completed_at, TIMESTAMPTZ 'epoch')::timestamptz AS ts,
       CASE WHEN r.id IS NULL THEN '__unknown__'
            WHEN r.daemon_id IS NULL OR r.daemon_id = '' THEN '__cloud__'
            ELSE r.daemon_id END AS machine,
       CASE WHEN t.status = 'failed' THEN COALESCE(NULLIF(t.failure_reason, ''), 'unclassified') ELSE '' END AS failure_reason,
       count(*)::int AS task_count
FROM agent_task_queue t
JOIN agent a ON a.id = t.agent_id
LEFT JOIN agent_runtime r ON r.id = t.runtime_id
WHERE a.workspace_id = @workspace_id
  AND t.status IN ('completed', 'failed')
  AND t.completed_at >= @since::timestamptz AND t.completed_at < @until::timestamptz
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3;
