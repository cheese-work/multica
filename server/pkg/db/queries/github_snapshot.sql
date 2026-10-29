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
    snapshot_base_ref      = sqlc.narg('base_ref'),
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
-- whose snapshot is both stale and undecided. A decided snapshot leaves this
-- fast set; ClaimGitHubPRsDueForRecheck re-checks it on a slower TTL.
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

-- name: ClaimGitHubPRsDueForRecheck :many
-- CHE-1665: slow re-check of open/draft PRs whose snapshot, decided or not, is
-- older than the re-check TTL. A base moved by a direct push sends no
-- pull_request event, so a decided MERGEABLE snapshot can turn DIRTY unseen;
-- this sweep is what notices. A PR is due when both its last snapshot and its
-- last pick (snapshot_recheck_at) are older than the TTL. Picking stamps
-- snapshot_recheck_at, so a PR whose fetch fails waits one TTL before it is
-- picked again and cannot hold the per-installation or global cap. Least
-- recently touched first, at most per_installation addresses per installation
-- per batch, so one busy installation cannot starve the rest.
WITH candidates AS (
    SELECT pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number,
           MIN(GREATEST(pr.snapshot_fetched_at, pr.snapshot_recheck_at))::TIMESTAMPTZ AS last_touched_at,
           MIN(pr.snapshot_fetched_at)::TIMESTAMPTZ AS oldest_fetched_at
    FROM github_pull_request AS pr
    WHERE pr.state IN ('open', 'draft')
      AND pr.snapshot_fetched_at < sqlc.arg('older_than')
      AND (pr.snapshot_recheck_at IS NULL OR pr.snapshot_recheck_at < sqlc.arg('older_than'))
      AND EXISTS (
          SELECT 1
          FROM workspace w
          WHERE w.id = pr.workspace_id
            AND (w.settings ->> 'github_enabled') IS DISTINCT FROM 'false'
      )
    GROUP BY pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number
), ranked AS (
    SELECT c.*, ROW_NUMBER() OVER (
        PARTITION BY c.installation_id
        ORDER BY c.last_touched_at, c.repo_owner, c.repo_name, c.pr_number
    ) AS installation_rank
    FROM candidates AS c
), picked AS (
    SELECT r.installation_id, r.repo_owner, r.repo_name, r.pr_number, r.last_touched_at, r.oldest_fetched_at, r.installation_rank
    FROM ranked AS r
    WHERE r.installation_rank <= sqlc.arg('per_installation')::BIGINT
    ORDER BY r.installation_rank, r.last_touched_at
    LIMIT sqlc.arg('max_rows')
), stamped AS (
    UPDATE github_pull_request AS pr
    SET snapshot_recheck_at = sqlc.arg('picked_at')
    FROM picked AS p
    WHERE pr.installation_id = p.installation_id
      AND pr.repo_owner = p.repo_owner
      AND pr.repo_name = p.repo_name
      AND pr.pr_number = p.pr_number
      AND pr.state IN ('open', 'draft')
    RETURNING pr.id
)
SELECT installation_id, repo_owner, repo_name, pr_number, oldest_fetched_at
FROM picked
ORDER BY installation_rank, last_touched_at;
-- name: ListStaleConflictWatchedGitHubPRs :many
-- Second sweep source: open/draft PRs linked to an issue with an enabled
-- "wake on merge conflict" rule. A decided snapshot leaves the undecided sweep,
-- but a base-branch move can turn it conflicting without any event for this
-- PR, so watched PRs are re-fetched on the TTL regardless. The candidate set
-- comes from the enabled rules, whose count is capped. Like the undecided
-- sweep, an address cursor rotates the bounded batch, so addresses whose
-- fetches keep failing (their snapshot never gets newer) cannot pin it.
WITH candidates AS (
    SELECT pr.installation_id, pr.repo_owner, pr.repo_name, pr.pr_number
    FROM github_pull_request AS pr
    WHERE pr.state IN ('open', 'draft')
      AND (pr.snapshot_fetched_at IS NULL OR pr.snapshot_fetched_at < sqlc.arg('older_than'))
      AND pr.id IN (
          SELECT ipr.pull_request_id
          FROM issue_wakeup AS w
          JOIN issue_pull_request AS ipr ON ipr.issue_id = w.issue_id
          WHERE w.enabled
            AND w.condition->>'type' = 'pull_request'
            AND w.condition->>'event' = 'conflicts'
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

-- name: ListConflictWatchedGitHubPRNumbersInRepo :many
-- Open/draft PRs in one repository that an enabled "wake on merge conflict"
-- rule watches. A merge into the repository can make them conflicting without
-- an event of their own, so the merge webhook re-fetches them.
SELECT DISTINCT pr.pr_number
FROM issue_wakeup AS w
JOIN issue_pull_request AS ipr ON ipr.issue_id = w.issue_id
JOIN github_pull_request AS pr ON pr.id = ipr.pull_request_id
WHERE w.enabled
  AND w.condition->>'type' = 'pull_request'
  AND w.condition->>'event' = 'conflicts'
  AND pr.installation_id = $1 AND pr.repo_owner = $2 AND pr.repo_name = $3
  AND pr.state IN ('open', 'draft')
  AND pr.pr_number <> $4
ORDER BY pr.pr_number
LIMIT 200;

-- name: ListGitHubPRNumbersByHeadSHA :many
-- Resolves a commit SHA to the PR numbers whose head it is. `status` webhook
-- events (legacy commit statuses) carry a SHA + repo but no PR number, so we
-- map back through the mirrored head_sha to find which PR(s) to refresh.
SELECT DISTINCT pr_number
FROM github_pull_request
WHERE installation_id = $1 AND repo_owner = $2 AND repo_name = $3 AND head_sha = $4;

-- name: GetGitHubPullRequestByID :one
SELECT * FROM github_pull_request WHERE id = $1;

-- name: InvalidateOpenGitHubPRMergeStateOnBase :many
-- CHE-1417: a merge moves its base branch, which can make other open PRs on
-- that base DIRTY or BEHIND without any pull_request event of their own. Their
-- stored merge state no longer describes the base, so it is cleared: the
-- merge handler refreshes the returned PRs, and one the refresh queue drops is
-- undecided, so ListStaleUndecidedGitHubPRs picks it up. A row whose base is
-- not known yet (no snapshot since the column was added) is included.
UPDATE github_pull_request
SET api_mergeable          = NULL,
    api_merge_state_status = NULL,
    updated_at             = now()
WHERE installation_id = sqlc.arg('installation_id')
  AND repo_owner = sqlc.arg('repo_owner')
  AND repo_name = sqlc.arg('repo_name')
  AND state IN ('open', 'draft')
  AND pr_number <> sqlc.arg('merged_pr_number')
  AND (snapshot_base_ref = sqlc.arg('base_ref') OR snapshot_base_ref IS NULL)
RETURNING installation_id, repo_owner, repo_name, pr_number;
