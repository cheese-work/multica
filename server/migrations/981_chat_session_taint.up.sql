-- CHE-3702: taint of the DM content a fork or continuation copied. Stored on
-- the chat that holds the copy, so deleting or unlinking the source cannot
-- erase it.
ALTER TABLE chat_session ADD COLUMN IF NOT EXISTS taint_ids text[] NOT NULL DEFAULT '{}';

-- taint_verified: the chat's taint_ids is complete. The default is false, so a
-- row inserted by a binary that predates this column stays unverified and the
-- read side can tell that its copied history was never accounted for.
ALTER TABLE chat_session ADD COLUMN IF NOT EXISTS taint_verified boolean NOT NULL DEFAULT false;

-- Backfill existing forks and continuations from their live ancestry.
WITH RECURSIVE ancestry(root, id) AS (
    SELECT c.id, c.id FROM chat_session c
    WHERE c.forked_from_session_id IS NOT NULL OR c.continued_from_session_id IS NOT NULL
    UNION
    SELECT a.root, p.id
    FROM ancestry a
    JOIN chat_session c ON c.id = a.id
    JOIN LATERAL unnest(ARRAY[c.forked_from_session_id, c.continued_from_session_id]) AS p(id) ON p.id IS NOT NULL
),
inherited AS (
    SELECT a.root, array_agg(DISTINCT x ORDER BY x) AS ids
    FROM ancestry a
    JOIN agent_task_queue q ON q.chat_session_id = a.id AND a.id <> a.root
    CROSS JOIN LATERAL unnest(q.taint_ids) AS x
    GROUP BY a.root
)
UPDATE chat_session c SET taint_ids = i.ids FROM inherited i WHERE i.root = c.id;

-- Verify every chat whose fork and continuation chain resolves to live rows
-- (no link at all counts as resolved). A chain with a deleted source stays
-- unverified: that taint was lost before this migration and is not recoverable.
WITH RECURSIVE ancestry(root, id) AS (
    SELECT c.id, c.id FROM chat_session c
    UNION
    SELECT a.root, p.id
    FROM ancestry a
    JOIN chat_session c ON c.id = a.id
    JOIN LATERAL unnest(ARRAY[c.forked_from_session_id, c.continued_from_session_id]) AS p(id) ON p.id IS NOT NULL
),
broken AS (
    SELECT DISTINCT a.root
    FROM ancestry a
    JOIN chat_session c ON c.id = a.id
    JOIN LATERAL unnest(ARRAY[c.forked_from_session_id, c.continued_from_session_id]) AS p(id) ON p.id IS NOT NULL
    WHERE NOT EXISTS (SELECT 1 FROM chat_session s WHERE s.id = p.id)
)
UPDATE chat_session c SET taint_verified = true
WHERE NOT EXISTS (SELECT 1 FROM broken b WHERE b.root = c.id);
