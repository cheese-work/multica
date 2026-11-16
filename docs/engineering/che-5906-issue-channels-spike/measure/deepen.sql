SET session_replication_role = replica;
WITH x AS (SELECT id, parent_id, lag(id) OVER (PARTITION BY parent_id ORDER BY created_at, id) prev FROM comment WHERE parent_id IS NOT NULL)
UPDATE comment c SET parent_id = x.prev FROM x WHERE c.id = x.id AND x.prev IS NOT NULL AND (hashtext(c.id::text) % 5) = 0;
ANALYZE comment;
SELECT count(*) AS depth_gt1 FROM comment r JOIN comment p ON p.id=r.parent_id WHERE p.parent_id IS NOT NULL;
