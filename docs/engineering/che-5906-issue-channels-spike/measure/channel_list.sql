-- Candidate channel-list query (plan 3.3) from existing rows only: comment authors + 'mentioned' inbox items.
-- :w workspace, :m member. The 'opened' mark has no existing store (see C1 note), so it is not in this query.
WITH RECURSIVE
cand AS (
  SELECT c.id AS cid, NULL::timestamptz AS tag_at, NULL::uuid AS tagger, true AS wrote
  FROM comment c
  WHERE c.workspace_id = :'w' AND c.author_type = 'member' AND c.author_id = :'m' AND c.deleted_at IS NULL
    AND c.created_at > now() - make_interval(days => :days)
  UNION ALL
  SELECT tc.id, tc.created_at, tc.author_id, false
  FROM inbox_item i JOIN comment tc ON tc.id = (i.details->>'comment_id')::uuid
  WHERE i.workspace_id = :'w' AND i.recipient_type = 'member' AND i.recipient_id = :'m' AND i.type = 'mentioned'
    AND i.created_at > now() - make_interval(days => :days)
),
cand_root AS MATERIALIZED (  -- walk each candidate to its thread root (GetThreadRoot semantics), one indexed walk per row
  SELECT r.id AS root, cand.tag_at, cand.tagger, cand.wrote
  FROM cand
  CROSS JOIN LATERAL (
    WITH RECURSIVE up(id, parent_id) AS (
      SELECT c.id, c.parent_id FROM comment c WHERE c.id = cand.cid
      UNION ALL SELECT p.id, p.parent_id FROM up JOIN comment p ON p.id = up.parent_id)
    SELECT id FROM up WHERE parent_id IS NULL LIMIT 1) r
),
roots AS MATERIALIZED (SELECT DISTINCT root FROM cand_root),
down AS MATERIALIZED (  -- all comments of each candidate thread
  SELECT r.root, c.id, c.created_at, c.author_id FROM roots r JOIN comment c ON c.id = r.root
  UNION ALL SELECT d.root, c.id, c.created_at, c.author_id FROM down d JOIN comment c ON c.parent_id = d.id
),
last_at AS (SELECT root, max(created_at) AS last_at FROM down GROUP BY root),
wrote AS (SELECT DISTINCT root FROM cand_root WHERE wrote),
replied AS (  -- tagged, and someone else commented after the tag
  SELECT DISTINCT cr.root FROM cand_root cr JOIN down d ON d.root = cr.root AND d.created_at > cr.tag_at AND d.author_id <> cr.tagger
  WHERE NOT cr.wrote
),
joined AS (
  SELECT l.root, l.last_at, (w.root IS NULL) AS waiting  -- waiting: joined by tag only (member has not written)
  FROM last_at l LEFT JOIN wrote w ON w.root = l.root
  WHERE w.root IS NOT NULL OR l.root IN (SELECT root FROM replied)
)
SELECT c.issue_id, count(*) AS threads, count(*) FILTER (WHERE j.waiting) AS waiting, max(j.last_at) AS last_at
FROM joined j JOIN comment c ON c.id = j.root
JOIN issue i ON i.id = c.issue_id AND i.workspace_id = :'w'
GROUP BY c.issue_id
ORDER BY (count(*) FILTER (WHERE j.waiting) > 0) DESC, max(j.last_at) DESC
LIMIT 50;
