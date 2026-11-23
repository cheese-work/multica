-- name: IssueWatchFence :one
-- Projection for one-shot `at --watch issue` wakeups (CHE-8945 plan r6, CHE-8961).
-- One statement serves the claim-time fence and, later, the fire-time compare.
-- run_id is NULL at claim, so no comment is credited there.
--
-- Work bound: `cs` is MATERIALIZED and has LIMIT 201 with no ORDER BY, so the
-- comment scan stops at 201 rows; the order is applied only inside string_agg.
-- 201 rows back means "over cap" (the caller treats it as no fence).
--
-- The row hash covers every issue column except revision (compared on its own)
-- and the three timestamps that a comment insert or a claim moves. A column
-- added later is covered without a code change.
--
-- `kept` drops R's own fresh comments only while they are pristine: every column
-- a writer that does not bump revision sets is still NULL (tripwire C8).
WITH cs AS MATERIALIZED (
  SELECT c.* FROM comment c WHERE c.issue_id = sqlc.arg('issue_id') LIMIT 201
), kept AS (
  SELECT cs.* FROM cs WHERE NOT (
    sqlc.narg('run_id')::uuid IS NOT NULL
    AND cs.source_task_id IS NOT DISTINCT FROM sqlc.narg('run_id')::uuid
    AND cs.author_type = 'agent' AND cs.author_id = sqlc.narg('agent_id')::uuid
    AND cs.created_at >= sqlc.narg('fence_at')::timestamptz AND cs.revision = 1
    AND cs.decision_intent_id IS NULL AND cs.decision_request_id IS NULL
    AND cs.decision_answer_id IS NULL AND cs.decision_comment_kind IS NULL
    AND cs.recovery_settled_at IS NULL AND cs.deleted_at IS NULL AND cs.resolved_at IS NULL)
)
SELECT md5((to_jsonb(i) - ARRAY['revision','updated_at','last_activity_at','first_executed_at']::text[])::text)::text AS row_hash,
       i.revision::bigint AS revision,
       now()::timestamptz AS read_at,
       (SELECT count(*) FROM cs)::bigint AS comment_rows,
       ((SELECT count(*) FROM cs) - (SELECT count(*) FROM kept))::bigint AS credit,
       (SELECT md5(COALESCE(string_agg(md5((to_jsonb(k) - 'updated_at')::text), ',' ORDER BY k.id), '')) FROM kept k)::text AS comment_hash,
       (EXISTS (SELECT 1 FROM issue_to_label l WHERE l.issue_id = i.id)
         OR EXISTS (SELECT 1 FROM attachment a WHERE a.issue_id = i.id)
         OR EXISTS (SELECT 1 FROM attachment a WHERE a.comment_id IN (SELECT id FROM cs))
         OR EXISTS (SELECT 1 FROM issue_reaction r WHERE r.issue_id = i.id)
         OR EXISTS (SELECT 1 FROM comment_reaction cr WHERE cr.comment_id IN (SELECT id FROM cs))
         OR EXISTS (SELECT 1 FROM issue_source_context s WHERE s.issue_id = i.id AND s.state = 'attached'))::boolean AS attached
FROM issue i WHERE i.id = sqlc.arg('issue_id');
