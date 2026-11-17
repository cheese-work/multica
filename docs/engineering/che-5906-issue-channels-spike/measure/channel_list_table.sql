SELECT tp.issue_id, count(*) AS threads, count(*) FILTER (WHERE tp.waiting) AS waiting, max(tp.last_activity_at) AS last_at
FROM thread_participant tp JOIN issue i ON i.id = tp.issue_id AND i.workspace_id = tp.workspace_id
WHERE tp.workspace_id = :'w' AND tp.member_id = :'m' AND tp.joined
GROUP BY tp.issue_id
ORDER BY (count(*) FILTER (WHERE tp.waiting) > 0) DESC, max(tp.last_activity_at) DESC
LIMIT 50;
