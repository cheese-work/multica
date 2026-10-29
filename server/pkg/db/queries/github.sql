-- =====================
-- GitHub Installation
-- =====================

-- name: ListGitHubInstallationsByWorkspace :many
SELECT * FROM github_installation
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: ListGitHubInstallationsByInstallationID :many
-- One installation_id can be bound to several workspaces; webhook routing lists
-- every binding and fans the event out to each bound workspace. Ordered oldest
-- first so processing is deterministic and replay-stable.
SELECT * FROM github_installation
WHERE installation_id = $1
ORDER BY created_at ASC, id ASC;

-- name: GetGitHubInstallationByID :one
SELECT * FROM github_installation
WHERE id = $1;

-- name: CreateGitHubInstallation :one
INSERT INTO github_installation (
    workspace_id, installation_id, account_login, account_type, account_avatar_url, connected_by_id
) VALUES (
    $1, $2, $3, $4, sqlc.narg('account_avatar_url'), sqlc.narg('connected_by_id')
)
ON CONFLICT (workspace_id, installation_id) DO UPDATE SET
    account_login = EXCLUDED.account_login,
    account_type = EXCLUDED.account_type,
    account_avatar_url = EXCLUDED.account_avatar_url,
    connected_by_id = EXCLUDED.connected_by_id,
    updated_at = now()
RETURNING *;

-- name: DeleteGitHubInstallation :exec
DELETE FROM github_installation WHERE id = $1 AND workspace_id = $2;

-- name: DeleteGitHubInstallationByInstallationID :many
-- GitHub-side uninstall/suspend removes trust in the installation entirely, so
-- drop every workspace binding. Returns one row per deleted binding so the
-- handler can broadcast to each affected workspace.
DELETE FROM github_installation WHERE installation_id = $1
RETURNING id, workspace_id;

-- name: UpdateGitHubInstallationAccountByInstallationID :many
-- Refresh the GitHub account display metadata across every workspace binding of
-- an installation (fired by installation.created/new_permissions_accepted/
-- unsuspend). Leaves workspace_id and connected_by_id untouched.
UPDATE github_installation
SET account_login = $2,
    account_type = $3,
    account_avatar_url = sqlc.narg('account_avatar_url'),
    updated_at = now()
WHERE installation_id = $1
RETURNING *;

-- name: UpsertPendingGitHubInstallation :one
INSERT INTO github_pending_installation (
    installation_id, account_login, account_type, account_avatar_url
) VALUES (
    $1, $2, $3, sqlc.narg('account_avatar_url')
)
ON CONFLICT (installation_id) DO UPDATE SET
    account_login = EXCLUDED.account_login,
    account_type = EXCLUDED.account_type,
    account_avatar_url = EXCLUDED.account_avatar_url,
    updated_at = now()
RETURNING *;

-- name: DeletePendingGitHubInstallation :exec
DELETE FROM github_pending_installation WHERE installation_id = $1;

-- name: GetPendingGitHubInstallation :one
SELECT * FROM github_pending_installation WHERE installation_id = $1
;

-- =====================
-- GitHub Pull Request
-- =====================

-- name: UpsertGitHubPullRequest :one
-- mergeable_state has three-state semantics on UPDATE:
--   1. clear_mergeable_state=true → write NULL (state-changing actions like
--      opened/synchronize/reopened/edited(base) invalidate the prior verdict).
--   2. clear_mergeable_state=false, mergeable_state non-null → write the value.
--   3. clear_mergeable_state=false, mergeable_state null → preserve existing
--      column. Metadata events (labeled/assigned/etc.) ship payloads without
--      mergeability, and silently clobbering a known clean/dirty would lose
--      information that GitHub only re-computes lazily.
-- state/merged_at follow the same "don't let a stale payload move things
-- backward" idea: `merged` is terminal, and a redelivered or out-of-order
-- opened/synchronize webhook (GitHub does not guarantee delivery order) must
-- never demote an already-merged PR back to open. `closed` is also protected
-- against a stale non-terminal payload — a redelivered/out-of-order `opened`
-- or `synchronize` webhook must not silently reopen a closed PR. The one
-- state `closed` must still yield to is a GENUINE reopen: GitHub's
-- action=reopened event, which derivePRState maps to state=open. Since the
-- state column alone can't distinguish "stale opened payload" from "genuine
-- reopened action" (both carry EXCLUDED.state='open'), the caller passes
-- is_reopen — true only when the webhook's action field is literally
-- "reopened" — as the discriminator (CHE-374 review round 3, item 1).
--
-- is_reopen alone is not enough: GitHub does not guarantee delivery order, so
-- a DELAYED redelivery of an earlier "reopened" webhook can arrive after a
-- second close was already recorded (close -> reopen -> close again ->
-- stale redelivery of the FIRST reopen). That stale payload also carries
-- is_reopen=true and, without an ordering check, would wrongly reopen a PR
-- that is genuinely closed again. closed_at is used as the ordering signal
-- (deliberately not pr_updated_at, which is a general activity clock bumped
-- by labels/comments/assignee changes too, not a state-transition clock): a
-- genuine, fresher reopen either clears closed_at (GitHub sets it to null on
-- reopen) or, for a same-delivery replay, carries a closed_at that is not
-- older than what is already stored. A reopen payload whose closed_at is
-- OLDER than the stored closed_at is describing an earlier close than the one
-- currently on file and is rejected as stale (CHE-374 review round 4, item 1).
-- INSERT path always writes the incoming value (NULL acceptable for a new row).
--
-- GitHub may deliver events out of order. An event older than the stored row
-- (pr_updated_at) updates nothing and returns no row, so a late "opened" can't
-- roll a merged PR back to open — the same guard UpsertVCSPullRequest has.
INSERT INTO github_pull_request (
    workspace_id, installation_id, repo_owner, repo_name, pr_number,
    title, state, html_url, branch, author_login, author_avatar_url,
    merged_at, closed_at, pr_created_at, pr_updated_at,
    head_sha, mergeable_state, head_repo_same,
    additions, deletions, changed_files
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, sqlc.narg('branch'), sqlc.narg('author_login'), sqlc.narg('author_avatar_url'),
    sqlc.narg('merged_at'), sqlc.narg('closed_at'), $9, $10,
    $11, sqlc.narg('mergeable_state'), sqlc.arg('head_repo_same'),
    $12, $13, $14
)
ON CONFLICT (workspace_id, repo_owner, repo_name, pr_number) DO UPDATE SET
    installation_id = EXCLUDED.installation_id,
    title = EXCLUDED.title,
    -- `merged` is always protected: once stored, only an incoming `merged`
    -- overwrites it. `closed` is protected UNLESS this delivery is a genuine,
    -- FRESH reopen: is_reopen=true AND the payload's closed_at is not older
    -- than the closed_at already stored (NULL closed_at in the payload also
    -- counts as fresh — that is what a real reopen looks like once GitHub
    -- clears the field). A stale redelivered "reopened" webhook whose
    -- closed_at is older than the stored value describes an earlier close and
    -- is treated the same as a non-reopen delivery: rejected (CHE-374 review
    -- round 4, item 1).
    --
    -- Accepted limitation (CHE-374 review round 5, item 1): a genuine reopen
    -- payload always carries closed_at=NULL, so the ordering check above
    -- (which requires both sides non-null) cannot compare two null-bearing
    -- reopens. A close -> reopen -> close sequence followed by an
    -- out-of-order redelivery of the FIRST reopen can therefore transiently
    -- reopen a PR that is genuinely closed again — closed_at cannot order
    -- two null-bearing events; that is a property of the signal, not this
    -- expression. A real fix needs a monotonic per-PR transition-ordering
    -- key (the delivery GUID is freshly minted per redelivery by GitHub, so
    -- it cannot serve as one) and is deliberately deferred: the next
    -- authoritative refresh (webhook, snapshot fetch, or TTL sweep)
    -- self-heals the mirror, so this is scoped out of this delivery rather
    -- than spending the remaining review ceiling on a narrow race.
    state = CASE
        WHEN github_pull_request.state = 'merged'
             AND EXCLUDED.state <> 'merged'
        THEN github_pull_request.state
        WHEN github_pull_request.state = 'closed'
             AND EXCLUDED.state <> 'merged'
             AND (
                 NOT COALESCE(sqlc.arg('is_reopen')::boolean, FALSE)
                 OR (
                     EXCLUDED.closed_at IS NOT NULL
                     AND github_pull_request.closed_at IS NOT NULL
                     AND EXCLUDED.closed_at < github_pull_request.closed_at
                 )
             )
        THEN github_pull_request.state
        ELSE EXCLUDED.state
    END,
    html_url = EXCLUDED.html_url,
    branch = EXCLUDED.branch,
    head_repo_same = EXCLUDED.head_repo_same,
    author_login = EXCLUDED.author_login,
    author_avatar_url = EXCLUDED.author_avatar_url,
    merged_at = CASE
        WHEN github_pull_request.state = 'merged'
             AND EXCLUDED.state <> 'merged'
        THEN github_pull_request.merged_at
        WHEN github_pull_request.state = 'closed'
             AND EXCLUDED.state <> 'merged'
             AND (
                 NOT COALESCE(sqlc.arg('is_reopen')::boolean, FALSE)
                 OR (
                     EXCLUDED.closed_at IS NOT NULL
                     AND github_pull_request.closed_at IS NOT NULL
                     AND EXCLUDED.closed_at < github_pull_request.closed_at
                 )
             )
        THEN github_pull_request.merged_at
        ELSE EXCLUDED.merged_at
    END,
    closed_at = CASE
        WHEN github_pull_request.state = 'closed'
             AND EXCLUDED.state <> 'merged'
             AND (
                 NOT COALESCE(sqlc.arg('is_reopen')::boolean, FALSE)
                 OR (
                     EXCLUDED.closed_at IS NOT NULL
                     AND github_pull_request.closed_at IS NOT NULL
                     AND EXCLUDED.closed_at < github_pull_request.closed_at
                 )
             )
        THEN github_pull_request.closed_at
        ELSE EXCLUDED.closed_at
    END,
    pr_updated_at = EXCLUDED.pr_updated_at,
    head_sha = EXCLUDED.head_sha,
    mergeable_state = CASE
        WHEN COALESCE(sqlc.narg('clear_mergeable_state')::boolean, FALSE) THEN NULL
        WHEN EXCLUDED.mergeable_state IS NOT NULL THEN EXCLUDED.mergeable_state
        ELSE github_pull_request.mergeable_state
    END,
    additions     = EXCLUDED.additions,
    deletions     = EXCLUDED.deletions,
    changed_files = EXCLUDED.changed_files,
    updated_at = now()
WHERE EXCLUDED.pr_updated_at >= github_pull_request.pr_updated_at
RETURNING *;

-- name: GetGitHubPullRequest :one
SELECT * FROM github_pull_request
WHERE workspace_id = $1 AND repo_owner = $2 AND repo_name = $3 AND pr_number = $4;

-- name: HasGitHubReviewWakeup :one
SELECT EXISTS (
    SELECT 1 FROM agent_task_queue
    WHERE issue_id = sqlc.arg(issue_id)
      AND context->>'github_review_key' = sqlc.arg(review_key)::text
);

-- name: RecordGitHubPRReview :exec
-- Keeps the latest approved, changes_requested or commented review of a PR; a
-- review that was submitted before the stored one is dropped.
UPDATE github_pull_request
SET review_state = sqlc.arg(review_state)::text,
    review_head_sha = sqlc.arg(head_sha)::text,
    review_id = sqlc.arg(review_id)::bigint,
    review_at = sqlc.arg(submitted_at)::timestamptz
WHERE id = sqlc.arg(id) AND (review_at IS NULL OR review_at <= sqlc.arg(submitted_at)::timestamptz);

-- name: ClearGitHubPRReview :exec
-- Withdraws the stored review when GitHub dismisses that exact review. A
-- dismissal of any other review leaves it alone. review_at stays as the
-- ordering watermark, so an older review delivered late is still dropped.
UPDATE github_pull_request
SET review_state = '', review_head_sha = '', review_id = 0
WHERE workspace_id = sqlc.arg(workspace_id)
  AND installation_id = sqlc.arg(installation_id)
  AND repo_owner = sqlc.arg(repo_owner)
  AND repo_name = sqlc.arg(repo_name)
  AND pr_number = sqlc.arg(pr_number)
  AND review_id = sqlc.arg(review_id)::bigint;

-- name: ListGitHubReviewAuthorCandidates :many
SELECT DISTINCT task.agent_id
FROM agent_task_queue task
JOIN agent author ON author.id = task.agent_id
JOIN github_pull_request pr ON pr.id = sqlc.arg(pull_request_id)
WHERE pr.workspace_id = sqlc.arg(workspace_id)
  AND author.workspace_id = pr.workspace_id
  AND (
    (sqlc.arg(task_key)::text <> '' AND right(replace(task.id::text, '-', ''), 12) = sqlc.arg(task_key)::text)
    OR (task.branch_name = pr.branch AND task.created_at <= pr.pr_created_at AND EXISTS (
        SELECT 1 FROM issue_pull_request link
        WHERE link.pull_request_id = pr.id AND link.issue_id = task.issue_id
    ))
  )
LIMIT 2;

-- name: MarkGitHubReviewWakeup :exec
UPDATE agent_task_queue
SET context = COALESCE(context, '{}'::jsonb) || jsonb_build_object('github_review_key', sqlc.arg(review_key)::text)
WHERE id = sqlc.arg(task_id) AND issue_id = sqlc.arg(issue_id);

-- name: ListPullRequestsByIssue :many
-- Returns the issue's linked PRs with the GitHub API snapshot (MUL-5265): the
-- mergeability verdict, the CI rollup, and per-check counts for the PR's
-- CURRENT snapshot head SHA. Checks are aggregated from
-- github_pull_request_check_run — the run-level snapshot written by the API
-- refresh pipeline — NOT the legacy suite-level webhook aggregation, which is
-- removed. The `issue_prs` CTE narrows to this issue's PR ids first so the
-- aggregation only touches check rows for those PRs. Rows for an OLD head are
-- excluded by the snapshot_head_sha filter. Both closing and reference links
-- remain visible; only closing links participate in merge automation.
WITH issue_prs AS (
    SELECT pr.id, pr.snapshot_head_sha
    FROM github_pull_request pr
    JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
    WHERE ipr.issue_id = sqlc.arg('issue_id')
),
checks AS (
    SELECT
        cr.pr_id,
        COUNT(*)::bigint AS total,
        SUM(CASE WHEN cr.status = 'completed' AND cr.conclusion IN
                ('failure','cancelled','timed_out','action_required','startup_failure','stale','error')
            THEN 1 ELSE 0 END)::bigint AS failed,
        SUM(CASE WHEN cr.status = 'completed' AND cr.conclusion IN
                ('success','neutral','skipped')
            THEN 1 ELSE 0 END)::bigint AS passed,
        SUM(CASE WHEN cr.status <> 'completed' OR cr.conclusion IS NULL
            THEN 1 ELSE 0 END)::bigint AS running,
        COALESCE(
            array_agg(cr.name) FILTER (WHERE cr.status = 'completed' AND cr.conclusion IN
                ('failure','cancelled','timed_out','action_required','startup_failure','stale','error')),
            '{}'
        )::text[] AS failed_names
    FROM github_pull_request_check_run cr
    JOIN issue_prs ip ON ip.id = cr.pr_id
    WHERE cr.head_sha = ip.snapshot_head_sha AND ip.snapshot_head_sha <> ''
    GROUP BY cr.pr_id
)
SELECT
    pr.id, pr.workspace_id, pr.installation_id, pr.repo_owner, pr.repo_name,
    pr.pr_number, pr.title, pr.state, pr.html_url, pr.branch, pr.author_login,
    pr.author_avatar_url, pr.merged_at, pr.closed_at, pr.pr_created_at,
    pr.pr_updated_at, pr.head_sha, pr.mergeable_state,
    pr.additions, pr.deletions, pr.changed_files,
    pr.api_mergeable, pr.api_merge_state_status, pr.checks_rollup_state,
    pr.snapshot_head_sha, pr.snapshot_fetched_at,
    pr.created_at, pr.updated_at,
    COALESCE(ipr.linked_by_type, 'system')::text AS linked_by_type,
    ipr.link_kind,
    COALESCE(c.total, 0)::bigint   AS checks_total,
    COALESCE(c.passed, 0)::bigint  AS checks_passed,
    COALESCE(c.failed, 0)::bigint  AS checks_failed,
    COALESCE(c.running, 0)::bigint AS checks_running,
    COALESCE(c.failed_names, '{}')::text[] AS failed_check_names
FROM github_pull_request pr
JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
LEFT JOIN checks c ON c.pr_id = pr.id
WHERE ipr.issue_id = sqlc.arg('issue_id')
ORDER BY pr.pr_created_at DESC;

-- name: GetIssueReviewHeadSha :one
-- Returns the head SHA of the commit currently "under review" for an issue:
-- the most-recently-updated linked PR that still has an open/draft state and a
-- non-empty head_sha. Used by the reviewer-loop dedup (TEN-356) so a pending
-- review task pinned to an old head does not satisfy a request after HEAD
-- advanced. Prefers in-flight PRs (open/draft) over merged/closed ones so a
-- stale merged sibling can't shadow the live review target; falls back to the
-- newest linked PR with a head_sha when none are open. Returns no rows (empty
-- string) when the issue has no linked PR — callers treat that as "no SHA key"
-- and dedup on (issue_id, agent_id) alone, preserving pre-TEN-356 behavior.
--
-- Spans both GitHub and self-hosted VCS PRs: a self-hosted PR pushing a new
-- commit must move the dedup head SHA the same way a GitHub PR does, otherwise
-- a fresh review round could be merged away against a stale key.
SELECT head_sha FROM (
    SELECT pr.head_sha AS head_sha, pr.state AS state, pr.pr_updated_at AS pr_updated_at
    FROM github_pull_request pr
    JOIN issue_pull_request ipr ON ipr.pull_request_id = pr.id
    WHERE ipr.issue_id = $1 AND pr.head_sha <> ''
    UNION ALL
    SELECT pr.head_sha AS head_sha, pr.state AS state, pr.pr_updated_at AS pr_updated_at
    FROM vcs_pull_request pr
    JOIN issue_vcs_pull_request ipr ON ipr.pull_request_id = pr.id
    WHERE ipr.issue_id = $1 AND pr.head_sha <> ''
) combined
ORDER BY (state IN ('open', 'draft')) DESC, pr_updated_at DESC
LIMIT 1;

-- name: ListIssueIDsForPullRequest :many
SELECT issue_id FROM issue_pull_request
WHERE pull_request_id = $1;

-- name: ListClosingIssueIDsForPullRequest :many
SELECT issue_id FROM issue_pull_request
WHERE pull_request_id = $1 AND link_kind = 'closing';

-- =====================
-- Issue ↔ Pull Request link
-- =====================

-- name: LinkIssueToPullRequest :execrows
-- Automatic link from a PR title, branch, or keyword. Returns 1 when the link
-- is new or its kind changes. Manual links are left untouched.
INSERT INTO issue_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id, link_kind
) VALUES (
    $1, $2, 'system', NULL, COALESCE(sqlc.narg('link_kind')::text, 'closing')
)
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    link_kind = EXCLUDED.link_kind
WHERE COALESCE(issue_pull_request.linked_by_type, 'system') <> 'member'
  AND issue_pull_request.link_kind IS DISTINCT FROM EXCLUDED.link_kind;

-- name: LinkIssueToPullRequestManually :execrows
-- A member linked this PR by hand. Marking an existing automatic link as
-- manual keeps a later title edit from removing it. Returns 1 when the row was
-- inserted or converted.
INSERT INTO issue_pull_request (
    issue_id, pull_request_id, linked_by_type, linked_by_id
) VALUES (
    $1, $2, 'member', sqlc.narg('linked_by_id')
)
ON CONFLICT (issue_id, pull_request_id) DO UPDATE SET
    linked_by_type = 'member',
    linked_by_id = EXCLUDED.linked_by_id,
    link_kind = 'closing'
WHERE issue_pull_request.linked_by_type IS DISTINCT FROM 'member'
   OR issue_pull_request.link_kind <> 'closing';

-- name: ListAutoLinkedIssueIDsForPullRequest :many
-- Issues this PR is linked to automatically. Manual links are not listed: the
-- webhook reconciles only what it created itself.
SELECT issue_id FROM issue_pull_request
WHERE pull_request_id = $1
  AND COALESCE(linked_by_type, 'system') <> 'member';

-- name: UnlinkIssueFromPullRequest :execrows
DELETE FROM issue_pull_request
WHERE issue_id = $1 AND pull_request_id = $2;

-- name: GetGitHubPullRequestInWorkspace :one
SELECT * FROM github_pull_request
WHERE id = $1 AND workspace_id = $2;

-- name: FindGitHubPullRequestByURL :one
-- Resolves a pasted PR URL to a mirrored PR. The caller normalizes the URL to
-- scheme://host/owner/repo/pull/N; html_url is stored in that shape.
SELECT * FROM github_pull_request
WHERE workspace_id = $1
  AND lower(rtrim(html_url, '/')) = lower(sqlc.arg('html_url')::text)
ORDER BY pr_updated_at DESC
LIMIT 1;

-- name: ListGitHubPRsWithUnverifiedHead :many
-- CHE-2301: PRs stored with head_repo_same=false. The row does not keep the head
-- repository id, so a repair must read GitHub to tell a real fork PR from a
-- same-repository PR that the old fork-flag check rejected. Rows whose PR was
-- last updated before the fork check shipped (CHE-2210, 2026-10-27) hold the
-- column default and were never judged. pr_updated_at only moves on webhook
-- writes; updated_at also moves on snapshot refresh, so it can't tell. One row
-- per PR address (workspaces mirror it), newest first, so a rate limit cuts off
-- the oldest.
SELECT installation_id, repo_owner, repo_name, pr_number
FROM github_pull_request
WHERE NOT head_repo_same AND pr_updated_at >= '2026-10-27T12:00:00Z'
GROUP BY installation_id, repo_owner, repo_name, pr_number
ORDER BY max(pr_updated_at) DESC, installation_id, repo_owner, repo_name, pr_number;
