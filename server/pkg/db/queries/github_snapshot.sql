-- =====================
-- GitHub API snapshot (MUL-5265, Plan C)
--
-- These queries back the API-snapshot refresh pipeline. The GitHub GraphQL
-- response is the single source of truth; each successful fetch is written as
-- one atomic batch replace (guarded update of the PR row + full replace of the
-- per-check rows) inside a single transaction.
-- =====================

-- name: ListGitHubPRRowsByAddress :many
-- One (installation, owner, repo, number) address can map to several
-- github_pull_request rows — the same installation can be bound to multiple
-- workspaces (#4823/#4855), each mirroring its own row. A single API fetch is
-- applied to every matching row (each guarded by its own head_sha).
--
-- CHE-374 review round 4, item 2: row selection is filtered to workspaces that
-- have not explicitly turned GitHub off (`settings->>'github_enabled' =
-- 'false'`), matching githubEnabledForWorkspace's "absent/unparseable =
-- enabled, only explicit false disables" semantics. The manager calls this
-- once BEFORE the outbound fetch (an empty result skips the fetch entirely)
-- and again AFTER the fetch to select rows to write, so a workspace whose
-- flag flips to disabled between those two calls is still excluded from the
-- write — the row selection step is the single place both checks share.
SELECT gpr.id, gpr.workspace_id, gpr.head_sha, gpr.state
FROM github_pull_request gpr
JOIN workspace w ON w.id = gpr.workspace_id
WHERE gpr.installation_id = $1
  AND gpr.repo_owner = $2
  AND gpr.repo_name = $3
  AND gpr.pr_number = $4
  AND (w.settings ->> 'github_enabled') IS DISTINCT FROM 'false';

-- name: UpdateGitHubPRSnapshot :execrows
-- Head-SHA anti-stale write (acceptance criterion 1): the snapshot is written
-- only when the row's current head_sha still equals the head the snapshot was
-- fetched for. If the head advanced (a newer push landed while this request was
-- in flight, mirrored by the pull_request webhook), 0 rows are updated and the
-- caller discards the whole response — the per-check replace is skipped too.
--
-- CHE-374 review round 5, item 2: the write also re-checks the workspace's
-- current github_enabled state, not just the head_sha. Manager.process
-- re-selects eligible rows once before this per-row loop (closing the
-- fetch-vs-write flip race), but a workspace can still flip to disabled
-- between that re-select and this specific row's write. Selection filters
-- eligibility; without this predicate the write did not. Uses the same
-- `IS DISTINCT FROM 'false'` form as ListGitHubPRRowsByAddress /
-- ListStaleUndecidedGitHubPRs (absent/unparseable = enabled, only explicit
-- false disables) so selection and write semantics stay identical.
UPDATE github_pull_request AS gpr
SET api_mergeable          = sqlc.narg('api_mergeable'),
    api_merge_state_status = sqlc.narg('api_merge_state_status'),
    checks_rollup_state    = sqlc.narg('checks_rollup_state'),
    snapshot_head_sha      = sqlc.arg('head_sha'),
    snapshot_fetched_at    = sqlc.arg('fetched_at'),
    updated_at             = now()
WHERE gpr.id = sqlc.arg('pr_id')
  AND gpr.head_sha = sqlc.arg('head_sha')
  AND EXISTS (
      SELECT 1 FROM workspace w
      WHERE w.id = gpr.workspace_id
        AND (w.settings ->> 'github_enabled') IS DISTINCT FROM 'false'
  );

-- name: DeleteGitHubPRCheckRuns :exec
-- First half of the atomic per-check replace. Runs inside the same transaction
-- as UpdateGitHubPRSnapshot and the inserts below.
DELETE FROM github_pull_request_check_run WHERE pr_id = $1;

-- name: InsertGitHubPRCheckRun :exec
INSERT INTO github_pull_request_check_run (
    pr_id, head_sha, ordinal, name, status, conclusion, details_url, is_status_context
) VALUES (
    $1, $2, $3, $4, $5, sqlc.narg('conclusion'), sqlc.narg('details_url'), $6
);

-- name: ListStaleUndecidedGitHubPRs :many
-- TTL / safety-net sweep source. Returns distinct addresses of open/draft PRs
-- whose snapshot is both stale and undecided. A decided snapshot leaves the
-- periodic refresh set; later webhook or view activity can still refresh it.
-- The caller advances an address cursor after each bounded batch. Rows after
-- the cursor sort first, followed by a wrap to the start, so even perpetually
-- failing addresses cannot pin the same first LIMIT rows forever.
--
-- CHE-374 review round 4, item 2: an address is only a sweep candidate when at
-- least one of its fan-out workspaces still has GitHub enabled. Filtering here
-- (rather than after Enqueue) means a master-off PR never enters the refresh
-- queue at all via the sweep path; Manager.process's own row-selection check
-- still applies before the outbound fetch as the second, non-sweep-specific
-- layer of defense.
WITH candidates AS (
    SELECT pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number
    FROM github_pull_request AS pr
    WHERE pr.state IN ('open', 'draft')
      AND (pr.snapshot_fetched_at IS NULL OR pr.snapshot_fetched_at < sqlc.arg('older_than'))
      AND (
          pr.snapshot_fetched_at IS NULL
          OR pr.api_mergeable IS NULL
          OR pr.api_mergeable = 'UNKNOWN'
          OR pr.checks_rollup_state IN ('PENDING', 'EXPECTED')
          OR EXISTS (
              SELECT 1
              FROM github_pull_request_check_run AS cr
              WHERE cr.pr_id = pr.id AND cr.status <> 'completed'
          )
      )
      AND EXISTS (
          SELECT 1
          FROM workspace w
          WHERE w.id = pr.workspace_id
            AND (w.settings ->> 'github_enabled') IS DISTINCT FROM 'false'
      )
    GROUP BY pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number
)
SELECT installation_id, repo_owner, repo_name, pr_number
FROM candidates
ORDER BY (
    ROW(installation_id, repo_owner, repo_name, pr_number) >
    ROW(
        sqlc.arg('after_installation_id')::BIGINT,
        sqlc.arg('after_repo_owner')::TEXT,
        sqlc.arg('after_repo_name')::TEXT,
        sqlc.arg('after_pr_number')::INTEGER
    )
) DESC,
installation_id, repo_owner, repo_name, pr_number
LIMIT sqlc.arg('max_rows');

-- name: ListGitHubPRNumbersByHeadSHA :many
-- Resolves a commit SHA to the PR numbers whose head it is. `status` webhook
-- events (legacy commit statuses) carry a SHA + repo but no PR number, so we
-- map back through the mirrored head_sha to find which PR(s) to refresh.
SELECT DISTINCT pr_number
FROM github_pull_request
WHERE installation_id = $1 AND repo_owner = $2 AND repo_name = $3 AND head_sha = $4;

-- name: GetGitHubPullRequestByID :one
SELECT * FROM github_pull_request WHERE id = $1;
