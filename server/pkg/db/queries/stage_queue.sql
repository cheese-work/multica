-- Stage queues (CHE-3611): the PRs in one slot owner's bottleneck stage. A slot
-- owner is one agent: the reviewer for the review stage, the lane's gate owner
-- for the device and browser stages. A PR holds a slot only while a run of the
-- owner is active on the stage's issue, so a waiting PR holds nothing.

-- name: LockStageQueue :exec
-- Serialises admissions into one owner's stage inside the enqueue transaction,
-- so two concurrent enqueues cannot both take the last slot.
SELECT pg_advisory_xact_lock(hashtextextended('stage_queue:' || sqlc.arg('stage')::text || ':' || sqlc.arg('agent_id')::uuid::text, 0));

-- name: MarkStageWaiting :exec
-- Records a refusal with who asked, the comment that asked, and the taint of
-- the refused run. A repeat refusal keeps the latest non-null actor and
-- comment and only adds taint.
INSERT INTO stage_queue_waiting (issue_id, agent_id, queued_at, actor_user_id, trigger_comment_id, taint_ids)
VALUES (sqlc.arg('issue_id')::uuid, sqlc.arg('agent_id')::uuid, now(), sqlc.narg('actor_user_id')::uuid, sqlc.narg('trigger_comment_id')::uuid, sqlc.arg('taint_ids')::text[])
ON CONFLICT (issue_id, agent_id) DO UPDATE SET
  queued_at = now(),
  actor_user_id = COALESCE(EXCLUDED.actor_user_id, stage_queue_waiting.actor_user_id),
  trigger_comment_id = COALESCE(EXCLUDED.trigger_comment_id, stage_queue_waiting.trigger_comment_id),
  taint_ids = (SELECT COALESCE(array_agg(DISTINCT t ORDER BY t), '{}')
                 FROM unnest(stage_queue_waiting.taint_ids || EXCLUDED.taint_ids) AS t);

-- name: GetStageWaiting :one
SELECT actor_user_id, trigger_comment_id, taint_ids FROM stage_queue_waiting
WHERE issue_id = sqlc.arg('issue_id')::uuid AND agent_id = sqlc.arg('agent_id')::uuid;

-- name: ListReviewStageEntries :many
-- Review stage: issues in a review status assigned to the reviewer, with a
-- linked open or draft PR (pr_key is the issue itself). An entry is listed when
-- it holds a slot, was refused and has not run since, or is the candidate being
-- admitted now.
SELECT i.id AS run_issue_id,
       i.id AS pr_key,
       (SELECT count(*) FROM issue g
         WHERE g.parent_issue_id = i.id AND g.title ILIKE '% gate:%'
           AND g.status NOT IN ('done', 'cancelled', 'agent_accepted', 'merged'))::int AS open_gates,
       COALESCE((SELECT min(pr.pr_created_at)
                   FROM issue_pull_request ipr
                   JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
                  WHERE ipr.issue_id = i.id), i.created_at)::timestamptz AS created_at,
       EXISTS (SELECT 1 FROM agent_task_queue t
                WHERE t.issue_id = i.id AND t.agent_id = sqlc.arg('agent_id')::uuid
                  AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')) AS active
FROM issue i
WHERE i.assignee_type = 'agent' AND i.assignee_id = sqlc.arg('agent_id')::uuid
  AND i.status IN ('agent_review', 'in_review')
  AND EXISTS (SELECT 1 FROM issue_pull_request ipr
                JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
               WHERE ipr.issue_id = i.id AND pr.state IN ('open', 'draft'))
  AND (i.id = sqlc.narg('candidate_id')::uuid
       OR EXISTS (SELECT 1 FROM agent_task_queue t
                   WHERE t.issue_id = i.id AND t.agent_id = sqlc.arg('agent_id')::uuid
                     AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory'))
       OR EXISTS (SELECT 1 FROM stage_queue_waiting w
                   WHERE w.issue_id = i.id AND w.agent_id = sqlc.arg('agent_id')::uuid
                     AND NOT EXISTS (SELECT 1 FROM agent_task_queue t
                                      WHERE t.issue_id = i.id AND t.agent_id = w.agent_id
                                        AND t.created_at >= w.queued_at)));

-- name: ListGateStageEntries :many
-- Device and browser stages: gate sub-issues titled "<stage> gate: <KEY>"
-- assigned to the lane's gate owner. pr_key is the parent issue, the PR's work
-- item; open_gates counts the parent's unfinished gate sub-issues. Listed on
-- the same terms as the review stage.
SELECT s.id AS run_issue_id,
       s.parent_issue_id AS pr_key,
       (SELECT count(*) FROM issue g
         WHERE g.parent_issue_id = s.parent_issue_id AND g.title ILIKE '% gate:%'
           AND g.status NOT IN ('done', 'cancelled', 'agent_accepted', 'merged'))::int AS open_gates,
       COALESCE((SELECT min(pr.pr_created_at)
                   FROM issue_pull_request ipr
                   JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
                  WHERE ipr.issue_id = s.parent_issue_id), p.created_at)::timestamptz AS created_at,
       EXISTS (SELECT 1 FROM agent_task_queue t
                WHERE t.issue_id = s.id AND t.agent_id = sqlc.arg('agent_id')::uuid
                  AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')) AS active
FROM issue s
JOIN issue p ON p.id = s.parent_issue_id
WHERE s.assignee_type = 'agent' AND s.assignee_id = sqlc.arg('agent_id')::uuid
  AND s.title ILIKE sqlc.arg('stage')::text || ' gate:%'
  AND s.status NOT IN ('done', 'cancelled', 'agent_accepted', 'merged')
  AND (s.id = sqlc.narg('candidate_id')::uuid
       OR EXISTS (SELECT 1 FROM agent_task_queue t
                   WHERE t.issue_id = s.id AND t.agent_id = sqlc.arg('agent_id')::uuid
                     AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory'))
       OR EXISTS (SELECT 1 FROM stage_queue_waiting w
                   WHERE w.issue_id = s.id AND w.agent_id = sqlc.arg('agent_id')::uuid
                     AND NOT EXISTS (SELECT 1 FROM agent_task_queue t
                                      WHERE t.issue_id = s.id AND t.agent_id = w.agent_id
                                        AND t.created_at >= w.queued_at)));

-- name: ListWaitingStageOwners :many
-- Every (stage, agent) with a refused PR still waiting: the release job's work
-- list. Whether a slot is free is decided per owner from the entries.
SELECT DISTINCT CASE
         WHEN i.title ILIKE 'device gate:%' THEN 'device'
         WHEN i.title ILIKE 'browser gate:%' THEN 'browser'
         ELSE 'review' END AS stage,
       w.agent_id
FROM stage_queue_waiting w
JOIN issue i ON i.id = w.issue_id
WHERE NOT EXISTS (SELECT 1 FROM agent_task_queue t
                   WHERE t.issue_id = w.issue_id AND t.agent_id = w.agent_id
                     AND t.created_at >= w.queued_at);
