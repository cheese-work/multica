-- CHE-8760 (CHE-8749 plan r4, S1): queue rank for the run queue order
-- project > parent issue > child issue. No caller uses these objects yet.
--
-- queue_rank_at / queue_rank return 0 unless workspace.settings.queue_order is
-- 'project_rank', so a 'priority' workspace (the default) sees no change. The
-- Go package server/internal/queuerank holds the same definition; its contract
-- test keeps them equal.
--
--   band  = urgent 4, high 3, medium 2, low 1, none 0
--   base  = 25 x project band + 5 x parent band + own band      (0 to 124)
--   rank  = base + 25 x floor(wait / aging)
--
-- project band: the issue's project priority; no project 0.
-- parent band : the immediate parent's priority; no parent = the issue's own.
-- no issue (DM, quick-create, run_only autopilot, unknown issue, issue of
-- another workspace): all three bands = fallback_priority, clamped to 0..4.
-- aging: workspace.settings.queue_aging_minutes, 5..720, default 30.

CREATE FUNCTION queue_rank_band(priority text) RETURNS integer
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE priority WHEN 'urgent' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 WHEN 'low' THEN 1 ELSE 0 END
$$;

-- p_at is the clock; queue_rank passes now(). Tests pass a fixed value.
CREATE FUNCTION queue_rank_at(
  p_workspace_id uuid, p_issue_id uuid, p_fallback_priority integer, p_since timestamptz, p_at timestamptz
) RETURNS integer
LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT COALESCE((
    SELECT CASE WHEN w.settings->>'queue_order' IS DISTINCT FROM 'project_rank' THEN 0
      ELSE 25 * b.project + 5 * b.parent + b.own
           + 25 * floor(greatest(extract(epoch FROM (p_at - COALESCE(p_since, p_at))), 0) / (60 * a.minutes))::integer
      END
    FROM workspace w
    LEFT JOIN issue i ON i.id = p_issue_id AND i.workspace_id = w.id
    LEFT JOIN project pr ON pr.id = i.project_id
    LEFT JOIN issue par ON par.id = i.parent_issue_id
    CROSS JOIN LATERAL (SELECT least(greatest(COALESCE(p_fallback_priority, 0), 0), 4) AS fb) f
    CROSS JOIN LATERAL (
      SELECT
        CASE WHEN i.id IS NULL THEN f.fb ELSE queue_rank_band(i.priority) END AS own,
        CASE WHEN i.id IS NULL THEN f.fb ELSE queue_rank_band(pr.priority) END AS project,
        CASE WHEN i.id IS NULL THEN f.fb
             WHEN par.id IS NULL THEN queue_rank_band(i.priority)
             ELSE queue_rank_band(par.priority) END AS parent
    ) b
    CROSS JOIN LATERAL (
      SELECT CASE WHEN w.settings->>'queue_aging_minutes' ~ '^[0-9]{1,6}$'
                  THEN least(greatest((w.settings->>'queue_aging_minutes')::integer, 5), 720)
                  ELSE 30 END AS minutes
    ) a
    WHERE w.id = p_workspace_id
  ), 0)
$$;

CREATE FUNCTION queue_rank(p_workspace_id uuid, p_issue_id uuid, p_fallback_priority integer, p_since timestamptz)
RETURNS integer
LANGUAGE sql STABLE PARALLEL SAFE AS $$
  SELECT queue_rank_at(p_workspace_id, p_issue_id, p_fallback_priority, p_since, now())
$$;

-- Slot-preserving re-rank (plan r4 Approach 8) for a list that spans workspaces.
-- Each candidate carries its global position under today's order (today_pos),
-- its rank from queue_rank, and a class (0 when the list has none; the one-shot
-- key of the due wakeup lane). The row whose pick = n takes the global position
-- of the row whose slot = n, same workspace and class:
--   slot = row_number() per (workspace, class) in today's order
--   pick = row_number() per (workspace, class) by rank DESC, then today's order
-- A 'priority' workspace has rank 0 on every row, so pick = slot and its rows
-- keep their rows and positions; a 'project_rank' workspace keeps its positions
-- and only changes which of its rows fill them. Callers ORDER BY pos.
CREATE TYPE queue_rank_cand AS (
  id uuid, workspace_id uuid, class integer, today_pos bigint, rank integer
);

CREATE FUNCTION queue_rerank_slots(cands queue_rank_cand[])
RETURNS TABLE (cand_id uuid, pos bigint)
LANGUAGE sql STABLE PARALLEL SAFE AS $$
  WITH c AS (
    SELECT u.id, u.workspace_id, u.class, u.today_pos, u.rank FROM unnest(cands) u
  ), s AS (
    SELECT c.*,
      row_number() OVER (PARTITION BY c.workspace_id, c.class ORDER BY c.today_pos) AS slot,
      row_number() OVER (PARTITION BY c.workspace_id, c.class ORDER BY c.rank DESC, c.today_pos) AS pick
    FROM c
  )
  SELECT picked.id, slotted.today_pos
  FROM s slotted
  JOIN s picked ON picked.workspace_id = slotted.workspace_id
               AND picked.class = slotted.class
               AND picked.pick = slotted.slot
$$;
