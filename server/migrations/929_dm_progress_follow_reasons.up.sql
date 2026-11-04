-- Follow provenance (CHE-3473 T3c, M12): a set of reasons per follow replaces the
-- single source and parent link. A reason is 'origin', 'manual', or
-- 'inherited:<issue id of the root the follow came from>'. An unfollowed row has
-- no reasons; it stays as the negative override against inheritance.
ALTER TABLE dm_progress_follow ADD COLUMN reasons TEXT[] NOT NULL DEFAULT '{}';

-- A child follow inherits from the root at the top of its parent_follow_id chain,
-- not from its immediate parent: a grandchild is removed by the root's unsubscribe.
WITH RECURSIVE chain AS (
    SELECT id AS follow_id, parent_follow_id, issue_id AS root_issue_id, 0 AS depth
    FROM dm_progress_follow
    WHERE source = 'child'
    UNION ALL
    SELECT c.follow_id, p.parent_follow_id, p.issue_id, c.depth + 1
    FROM chain c
    JOIN dm_progress_follow p ON p.id = c.parent_follow_id
),
roots AS (
    SELECT DISTINCT ON (follow_id) follow_id, root_issue_id
    FROM chain
    ORDER BY follow_id, depth DESC
)
UPDATE dm_progress_follow f
SET reasons = CASE
    WHEN f.state = 'unfollowed' THEN '{}'::text[]
    WHEN f.source = 'child' THEN ARRAY['inherited:' || (SELECT r.root_issue_id::text FROM roots r WHERE r.follow_id = f.id)]
    ELSE ARRAY[f.source]
END;

ALTER TABLE dm_progress_follow DROP COLUMN source, DROP COLUMN parent_follow_id;
