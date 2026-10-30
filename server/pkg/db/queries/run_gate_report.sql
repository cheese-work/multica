-- CHE-1919: read-only joins for the run gate offline report (CHE-1917 Slice 0b).
-- Every query reads; nothing here admits, folds or skips a run.

-- name: ListRunGateReportEvents :many
SELECT * FROM run_gate_event
WHERE workspace_id = @workspace_id AND event_time >= @since AND event_time < @until
ORDER BY event_time, id;

-- name: FirstRunGateEventTime :one
-- When capture started in this workspace; earlier windows predate the ledger.
SELECT min(event_time)::timestamptz AS first_event_time FROM run_gate_event WHERE workspace_id = @workspace_id;

-- name: ListRunGateRuns :many
-- Every run linked to a ledger event by native evidence only: the comment ids a
-- run was triggered by, coalesced or delivered, or the run a wakeup receipt
-- names. No timing is used. A cancelled duplicate keeps its trigger link, so one
-- event can list several runs.
SELECT e.id AS event_id, t.id AS task_id, t.status, COALESCE(t.failure_reason, '')::text AS failure_reason,
       t.created_at, t.dispatched_at, t.started_at, t.completed_at,
       (t.trigger_comment_id = e.trigger_comment_id)::bool AS link_trigger,
       COALESCE(e.trigger_comment_id = ANY(t.coalesced_comment_ids), false)::bool AS link_coalesced,
       COALESCE(e.trigger_comment_id = ANY(t.delivered_comment_ids), false)::bool AS link_delivered,
       false::bool AS link_receipt
FROM run_gate_event e
JOIN agent_task_queue t ON t.issue_id = e.issue_id AND t.agent_id = e.agent_id
 AND (t.trigger_comment_id = e.trigger_comment_id
      OR e.trigger_comment_id = ANY(t.coalesced_comment_ids)
      OR e.trigger_comment_id = ANY(t.delivered_comment_ids))
WHERE e.workspace_id = @workspace_id AND e.source = 'comment_leader'
  AND e.event_time >= @since AND e.event_time < @until
UNION ALL
SELECT e.id, t.id, t.status, COALESCE(t.failure_reason, '')::text,
       t.created_at, t.dispatched_at, t.started_at, t.completed_at,
       false, false, false, true
FROM run_gate_event e
JOIN issue_wakeup_receipt r ON r.id = e.receipt_id
JOIN agent_task_queue t ON t.id = r.task_id
WHERE e.workspace_id = @workspace_id AND e.source IN ('wakeup_pr_merged', 'wakeup_pr_checks_failed')
  AND e.event_time >= @since AND e.event_time < @until
ORDER BY 1, 3, 2;

-- name: ListRunGateReceipts :many
-- The receipt a wakeup event wrote. An event with a receipt_id and no row here
-- lost its receipt; the cause is not recorded (see ListRunGateRuleGoneEvents).
SELECT e.id AS event_id, r.task_id, r.processed_at, r.deferred_at,
       COALESCE(r.payload->>'outcome', '')::text AS outcome
FROM run_gate_event e
JOIN issue_wakeup_receipt r ON r.id = e.receipt_id
WHERE e.workspace_id = @workspace_id AND e.source IN ('wakeup_pr_merged', 'wakeup_pr_checks_failed')
  AND e.event_time >= @since AND e.event_time < @until;

-- name: ListRunGateRuleGoneEvents :many
-- Wakeup events whose rule row no longer exists. Deleting a rule deletes its
-- receipts, so these inputs are missing from the surviving-receipt denominator
-- and their receipt join is lost. The ledger has no foreign key and keeps them.
SELECT e.id AS event_id FROM run_gate_event e
WHERE e.workspace_id = @workspace_id AND e.source IN ('wakeup_pr_merged', 'wakeup_pr_checks_failed')
  AND e.event_time >= @since AND e.event_time < @until
  AND e.wakeup_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM issue_wakeup w WHERE w.id = e.wakeup_id);

-- name: ListRunGateRunMutations :many
-- What a run did. comment_posted, issue_created and wakeup_created are keyed by
-- the run id the producer stamped (attribution 'task_id'). status_changed and
-- assignee_changed rows carry no run id, so they are the agent's own activity
-- on the issue inside the run's start..end window, and 'window_ambiguous' when
-- another run of the same agent on the issue overlaps that instant. A rule
-- deleted since has no row left to count.
SELECT task_id, kind, attribution, count(*)::bigint AS n FROM (
  SELECT source_task_id AS task_id, 'comment_posted'::text AS kind, 'task_id'::text AS attribution
  FROM comment WHERE source_task_id = ANY(@task_ids::uuid[])
  UNION ALL
  SELECT origin_id, 'issue_created', 'task_id'
  FROM issue WHERE origin_type = 'agent_create' AND origin_id = ANY(@task_ids::uuid[])
  UNION ALL
  SELECT source_task_id, 'wakeup_created', 'task_id'
  FROM issue_wakeup WHERE source_task_id = ANY(@task_ids::uuid[])
  UNION ALL
  SELECT t.id, a.action,
         CASE WHEN EXISTS (
           SELECT 1 FROM agent_task_queue o
           WHERE o.id <> t.id AND o.agent_id = t.agent_id AND o.issue_id = t.issue_id
             AND COALESCE(o.started_at, o.dispatched_at) <= a.created_at
             AND COALESCE(o.completed_at, now()) >= a.created_at
         ) THEN 'window_ambiguous' ELSE 'window' END
  FROM agent_task_queue t
  JOIN activity_log a ON a.issue_id = t.issue_id AND a.actor_type = 'agent' AND a.actor_id = t.agent_id
   AND a.action IN ('status_changed', 'assignee_changed')
   AND a.created_at >= COALESCE(t.started_at, t.dispatched_at)
   AND a.created_at <= COALESCE(t.completed_at, now())
  WHERE t.id = ANY(@task_ids::uuid[])
) m
GROUP BY task_id, kind, attribution
ORDER BY task_id, kind, attribution;

-- name: CountRunGateCommentEvidence :one
-- Producer-side denominator for comment_leader: comments in the window that a
-- squad-leader run is recorded against (as trigger, coalesced or delivered),
-- and how many of those have a ledger row for that leader. A comment that
-- resolved to a leader but never produced a run is invisible here; the figure
-- is a floor, not the producer volume.
SELECT count(*)::bigint AS evidenced, count(e.id)::bigint AS captured FROM (
  SELECT DISTINCT t.agent_id, c.id AS comment_id
  FROM agent_task_queue t
  JOIN comment c ON c.issue_id = t.issue_id AND c.workspace_id = @workspace_id
   AND c.created_at >= @since AND c.created_at < @until
   AND (c.id = t.trigger_comment_id OR c.id = ANY(t.coalesced_comment_ids) OR c.id = ANY(t.delivered_comment_ids))
  WHERE t.is_leader_task
) x
LEFT JOIN run_gate_event e ON e.workspace_id = @workspace_id AND e.source = 'comment_leader'
 AND e.event_key = x.comment_id::text AND e.candidate_key = x.agent_id::text;

-- name: CountRunGateWakeupEvidence :many
-- Producer-side denominator for the PR wakeup families: receipts still present,
-- and how many have a ledger row. A floor, never the producer volume: receipts
-- are deleted with their rule (rule or issue deletion, at any age) and 7 days
-- after processing, and no row records either deletion.
SELECT r.event_type, count(*)::bigint AS evidenced, count(e.id)::bigint AS captured
FROM issue_wakeup_receipt r
JOIN issue_wakeup w ON w.id = r.wakeup_id
LEFT JOIN run_gate_event e ON e.receipt_id = r.id
WHERE w.workspace_id = @workspace_id AND r.event_type IN ('pr.merged', 'pr.checks_failed')
  AND w.system_rule IN ('pr_merged', 'pr_checks_failed')
  AND r.created_at >= @since AND r.created_at < @until
GROUP BY r.event_type
ORDER BY r.event_type;
