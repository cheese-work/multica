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
UPDATE agent_task_queue SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('github_dispatch', sqlc.arg(source)::text)
WHERE id = sqlc.arg(task_id) AND issue_id = sqlc.arg(issue_id);

-- name: ListGitHubTriggeredActiveTasks :many
-- Live runs started by a GitHub event inside a scope: a PR system wakeup
-- (pr_merged, pr_checks_*, pr_needs_attention), a PR review wakeup, or a CI-green
-- workflow handoff (marked by MarkGitHubDispatchRun). The scope is matched against the repos of the issue's linked PRs.
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
  AND EXISTS (
    SELECT 1 FROM issue_pull_request link
    JOIN github_pull_request pr ON pr.id = link.pull_request_id
    WHERE link.issue_id = i.id
      AND sqlc.arg(scope)::text IN ('*', lower(pr.repo_owner), lower(pr.repo_owner || '/' || pr.repo_name))
  );
