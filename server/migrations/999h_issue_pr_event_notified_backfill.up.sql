-- Stops the live FYI floods (CHE-5901). A next_turn event whose member notice
-- is already on the timeline becomes notified, and the older duplicate notice
-- rows of one (issue, event) are archived: the newest stays.
SET LOCAL lock_timeout = '2s';

UPDATE issue_pr_event e
SET status = 'notified'
WHERE e.status = 'next_turn'
  AND EXISTS (
      SELECT 1 FROM activity_log a
      WHERE a.issue_id = e.issue_id
        AND a.action = 'wakeup_triggered'
        AND a.details->>'outcome' = 'notified'
        AND a.details->>'event_id' = e.id::text);

UPDATE inbox_item i
SET archived = true
FROM (
    SELECT id, row_number() OVER (
        PARTITION BY issue_id, details->>'event_id'
        ORDER BY created_at DESC, id DESC) AS newest
    FROM inbox_item
    WHERE type = 'agent_blocked'
      AND details->>'outcome' = 'notified'
      AND details->>'event_id' IS NOT NULL
) ranked
WHERE i.id = ranked.id AND ranked.newest > 1 AND i.archived = false;
