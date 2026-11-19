DROP FUNCTION IF EXISTS dm_progress_capture(uuid, text, text);
CREATE FUNCTION dm_progress_capture(p_issue uuid, p_kind text, p_to_status text) RETURNS void LANGUAGE sql AS $$
 WITH ev AS MATERIALIZED (SELECT gen_random_uuid() AS id)
 INSERT INTO dm_progress_pending(workspace_id, lineage_id, issue_id, event_kind, event_id, immediate)
 SELECT f.workspace_id, f.lineage_id, f.issue_id, p_kind, ev.id,
        p_kind = 'status'
        AND EXISTS (SELECT 1 FROM unnest(f.reasons) r WHERE r NOT LIKE 'inherited:%')
        AND ((p_to_status = 'blocked' AND f.last_rendered_status <> 'blocked') OR p_to_status IN ('done', 'cancelled'))
 FROM dm_progress_follow f, ev
 WHERE f.issue_id = p_issue AND f.state = 'active' AND f.muted_at IS NULL
$$;
