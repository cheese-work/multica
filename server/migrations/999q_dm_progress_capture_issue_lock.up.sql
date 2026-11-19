-- DM progress cards (CHE-3549, T3d): capture takes the issue FOR KEY SHARE before
-- it reads the follows. A follow write holds the issue FOR UPDATE until it
-- commits, so a capture (a PR link or state change writes no issue row, so it
-- took no lock before) waits for an open follow and then sees it: no event
-- commits after the follow's watermark and is missed. The INSERT is its own
-- statement, so it reads the follows after the wait. Status and assignee
-- writes already hold the row lock; this changes nothing for them.
CREATE OR REPLACE FUNCTION dm_progress_capture(p_issue uuid, p_kind text, p_to_status text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
 PERFORM 1 FROM issue WHERE id = p_issue FOR KEY SHARE;
 WITH ev AS MATERIALIZED (SELECT gen_random_uuid() AS id)
 INSERT INTO dm_progress_pending(workspace_id, lineage_id, issue_id, event_kind, event_id, immediate)
 SELECT f.workspace_id, f.lineage_id, f.issue_id, p_kind, ev.id,
        p_kind = 'status'
        AND EXISTS (SELECT 1 FROM unnest(f.reasons) r WHERE r NOT LIKE 'inherited:%')
        AND ((p_to_status = 'blocked' AND f.last_rendered_status <> 'blocked') OR p_to_status IN ('done', 'cancelled'))
 FROM dm_progress_follow f, ev
 WHERE f.issue_id = p_issue AND f.state = 'active' AND f.muted_at IS NULL;
END
$$;
