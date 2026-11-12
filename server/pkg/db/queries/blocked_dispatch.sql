-- name: GetBlockedDispatchFingerprint :one
SELECT jsonb_build_array(
    COALESCE(i.metadata->>'blocked_reason', ''),
    COALESCE((
        SELECT md5(string_agg(jsonb_build_array(p.id, p.head_sha, p.checks_rollup_state)::text, ';' ORDER BY p.id))
        FROM issue_pull_request ip JOIN github_pull_request p ON p.id = ip.pull_request_id
        WHERE ip.issue_id = i.id
    ), ''),
    COALESCE((
        SELECT md5(string_agg(jsonb_build_array(c.id, c.content, c.updated_at)::text, ';' ORDER BY c.created_at, c.id))
        FROM comment c WHERE c.issue_id = i.id AND c.author_type IN ('member', 'agent')
          AND c.author_id IS DISTINCT FROM sqlc.arg(tofu_id)::uuid
    ), '')
)::text
FROM issue i WHERE i.id = sqlc.arg(issue_id);

-- name: GetLatestCompletedBlockedDispatchTask :one
SELECT id, COALESCE(context->>'blocked_dispatch_fingerprint', '')::text AS fingerprint
FROM agent_task_queue
WHERE issue_id = sqlc.arg(issue_id) AND agent_id = sqlc.arg(agent_id) AND status = 'completed'
ORDER BY completed_at DESC LIMIT 1;

-- name: StampBlockedDispatchFingerprint :exec
UPDATE agent_task_queue
SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('blocked_dispatch_fingerprint', sqlc.arg(fingerprint)::text)
WHERE id = sqlc.arg(task_id);
