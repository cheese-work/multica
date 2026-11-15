-- name: GitHubDispatchHalted :one
-- True when a halt row covers the repo: the whole workspace, its owner, or the repo itself.
SELECT EXISTS (
    SELECT 1 FROM github_dispatch_halt
    WHERE workspace_id = sqlc.arg(workspace_id)
      AND scope IN ('*', lower(sqlc.arg(repo_owner)::text), lower(sqlc.arg(repo_owner)::text || '/' || sqlc.arg(repo_name)::text))
);

-- name: ListGitHubDispatchHalts :many
SELECT * FROM github_dispatch_halt WHERE workspace_id = $1 ORDER BY scope;

-- name: HaltGitHubDispatch :exec
INSERT INTO github_dispatch_halt (workspace_id, scope, halted_by)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id, scope) DO NOTHING;

-- name: ResumeGitHubDispatch :execrows
DELETE FROM github_dispatch_halt WHERE workspace_id = $1 AND scope = $2;

-- name: MarkGitHubDispatchRun :exec
-- Tags one run as started by a GitHub event, for a workflow handoff that
-- carries no wakeup or review marker of its own. The caller names the exact
-- task its enqueue created, so a later run of the issue is never tagged.
-- A non-empty repo ("owner/repo") records the repo that triggered the run, so the
-- sweep matches that repo even after the issue's PR links change.
UPDATE agent_task_queue SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('github_dispatch', sqlc.arg(source)::text)
  || CASE WHEN sqlc.arg(repo)::text = '' THEN '{}'::jsonb ELSE jsonb_build_object('github_dispatch_repo', lower(sqlc.arg(repo)::text)) END
WHERE id = sqlc.arg(task_id) AND issue_id = sqlc.arg(issue_id);

-- name: ListGitHubTriggeredActiveTasks :many
-- Live runs started by a GitHub event inside a scope, created before the halt that asked for the sweep
-- (a sweep that outlives its lease then never reaches a run started after a restore): a PR system wakeup
-- (pr_merged, pr_checks_*, pr_needs_attention), a PR review wakeup, or a CI-green
-- workflow handoff (marked by MarkGitHubDispatchRun). The scope is matched against
-- the repo that triggered the run when recorded, else the repos of the issue's linked PRs.
SELECT t.id FROM agent_task_queue t
JOIN issue i ON i.id = t.issue_id
WHERE i.workspace_id = sqlc.arg(workspace_id)
  AND t.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory')
  AND (
    t.context ? 'github_review_key'
    OR t.context ? 'github_dispatch'
    OR (t.trigger_evidence_kind = 'issue_wakeup' AND EXISTS (
        SELECT 1 FROM issue_wakeup w
        WHERE w.id = t.trigger_evidence_ref_id AND w.system_rule LIKE 'pr\_%'))
  )
  AND CASE
    WHEN t.context ? 'github_dispatch_repo' THEN
      sqlc.arg(scope)::text IN ('*', split_part(t.context->>'github_dispatch_repo', '/', 1), t.context->>'github_dispatch_repo')
    ELSE EXISTS (
      SELECT 1 FROM issue_pull_request link
      JOIN github_pull_request pr ON pr.id = link.pull_request_id
      WHERE link.issue_id = i.id
        AND sqlc.arg(scope)::text IN ('*', lower(pr.repo_owner), lower(pr.repo_owner || '/' || pr.repo_name))
    )
  END
  AND t.created_at < sqlc.arg(before)::timestamptz;

-- name: LockGitHubDispatchHaltShared :exec
-- Held by a dispatch until its transaction ends, so a switch change cannot slip
-- between the dispatch's check and its commit.
SELECT pg_advisory_xact_lock_shared(hashtextextended('github_dispatch_halt:' || sqlc.arg(workspace_id)::uuid::text, 0));

-- name: LockGitHubDispatchHaltExclusive :exec
-- Taken by a switch change; waits for in-flight dispatches and blocks new ones until it commits.
SELECT pg_advisory_xact_lock(hashtextextended('github_dispatch_halt:' || sqlc.arg(workspace_id)::uuid::text, 0));

-- name: HasUnfinishedGitHubDispatchHaltSweep :one
-- Read by a restore under the exclusive lock, after it reran the sweeps it knew of: a sweep still
-- pending or failed now means a halt committed in between, and the restore is refused.
SELECT EXISTS (SELECT 1 FROM github_dispatch_halt_audit WHERE workspace_id = $1 AND halted AND (sweep_pending OR sweep_failed));

-- name: ListAbandonedGitHubDispatchHaltSweeps :many
-- A sweep that failed, or is still pending after ten minutes (a crash), is unfinished: the next
-- restore runs it again first. A successful rerun clears sweep_failed and sweep_pending.
SELECT id, scope, created_at FROM github_dispatch_halt_audit
WHERE workspace_id = $1 AND halted AND (sweep_failed OR (sweep_pending AND created_at <= now() - interval '10 minutes'))
ORDER BY created_at;

-- name: RecordGitHubDispatchHaltAudit :one
-- created_at is the clock at the insert, under the exclusive admission lock: every admission that
-- started before the halt finished before it, so the sweep's cutoff covers all their runs.
INSERT INTO github_dispatch_halt_audit (workspace_id, scope, halted, actor_id, actor_source, sweep_pending, created_at)
VALUES ($1, $2, $3, $4, $5, $3, clock_timestamp())
RETURNING id, created_at;

-- name: SetGitHubDispatchHaltAuditSweep :exec
UPDATE github_dispatch_halt_audit SET runs_revoked = $2, sweep_failed = $3, sweep_pending = false WHERE id = $1;

-- name: ListGitHubDispatchHaltAudit :many
SELECT * FROM github_dispatch_halt_audit WHERE workspace_id = $1 ORDER BY created_at DESC, id DESC LIMIT 20;
