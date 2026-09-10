-- =====================
-- GitHub Merge Announcement
-- =====================
--
-- Durable per-merge record so every linked GitHub PR merge produces exactly
-- one platform comment on its owning issue, independent of completion
-- (CHE-374/CHE-379). See 466_github_merge_announcement.up.sql for the schema
-- rationale and 467_github_merge_announcement_identity_uidx.up.sql for the
-- identity/dedup key.

-- name: CreateGitHubMergeAnnouncement :one
-- Persisted in the same request as the merge/link write (github.go's
-- mirrorPullRequestForWorkspace), before HTTP 202 is returned, so no
-- in-memory enqueue can be lost after commit. ON CONFLICT DO NOTHING against
-- the identity index makes a redelivered webhook (new delivery_guid, same
-- merge) a no-op here rather than a second pending row; RETURNING nothing on
-- conflict lets the caller detect "already have one" and skip re-enqueueing
-- without a second round trip.
INSERT INTO github_merge_announcement (
    workspace_id, provider, repository_id, repo_owner, repo_name, pr_number,
    pull_request_id, issue_id, event_kind, delivery_guid, merge_commit_sha, merged_at,
    html_url, close_intent
) VALUES (
    sqlc.arg('workspace_id'), sqlc.arg('provider'), sqlc.arg('repository_id'),
    sqlc.arg('repo_owner'), sqlc.arg('repo_name'), sqlc.arg('pr_number'),
    sqlc.arg('pull_request_id'), sqlc.arg('issue_id'), sqlc.arg('event_kind'),
    sqlc.narg('delivery_guid'), sqlc.arg('merge_commit_sha'), sqlc.arg('merged_at'),
    sqlc.narg('html_url'), sqlc.narg('close_intent')
)
ON CONFLICT (workspace_id, provider, repository_id, pr_number, issue_id, event_kind) DO NOTHING
RETURNING *;

-- name: GetGitHubMergeAnnouncementByIdentity :one
-- Read-after-attempted-insert: distinguishes "this call created the pending
-- record" (CreateGitHubMergeAnnouncement returned a row) from "a prior
-- delivery already owns this identity" (conflict, 0 rows) so callers can log
-- the existing record's state without treating a duplicate merge event as an
-- error.
SELECT * FROM github_merge_announcement
WHERE workspace_id = sqlc.arg('workspace_id')
  AND provider = sqlc.arg('provider')
  AND repository_id = sqlc.arg('repository_id')
  AND pr_number = sqlc.arg('pr_number')
  AND issue_id = sqlc.arg('issue_id')
  AND event_kind = sqlc.arg('event_kind');

-- name: ClaimPendingGitHubMergeAnnouncement :one
-- Claims one due pending record for delivery. SKIP LOCKED lets concurrent
-- reconciliation sweepers and the inline post-write attempt run without
-- blocking each other; the lease makes a crashed claim visible to the next
-- sweep pass once it expires. Mirrors ClaimQueuedWebhookDelivery's shape
-- (server/pkg/db/queries/webhook_delivery.sql), reviewed under CHE-379.
WITH candidate AS (
    SELECT id
    FROM github_merge_announcement
    WHERE status = 'pending'
      AND available_at <= now()
      AND (lease_expires_at IS NULL OR lease_expires_at <= now())
    ORDER BY available_at, created_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE github_merge_announcement AS a
SET lease_token = gen_random_uuid(),
    lease_expires_at = now() + interval '2 minutes'
FROM candidate
WHERE a.id = candidate.id
RETURNING a.*;

-- name: CompleteGitHubMergeAnnouncementDelivery :one
-- Marks the record delivered and records the created comment's id in the
-- same statement. Scoped by lease_token so a worker that outlived its lease
-- cannot stomp a newer claimant's result (handleWebhookLeaseMutation-style
-- race handling in the caller).
UPDATE github_merge_announcement
SET status = 'delivered',
    comment_id = sqlc.arg('comment_id'),
    delivered_at = now(),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'pending'
RETURNING *;

-- name: RetryGitHubMergeAnnouncement :one
-- Records a transient delivery failure and makes the record eligible again
-- after backoff. Sanitized error only — see MERGE-05 / review condition A:
-- the last_error text must never carry secrets, and the caller is
-- responsible for sanitizing before this call.
UPDATE github_merge_announcement
SET attempt_count = attempt_count + 1,
    last_error = sqlc.arg('last_error'),
    available_at = sqlc.arg('available_at'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'pending'
RETURNING *;

-- name: FailGitHubMergeAnnouncement :one
-- Terminal failure after exhausting attempts. Left status='failed' (not
-- 'pending') so the reconciliation sweeper stops claiming it, while
-- last_error/attempt_count stay visible through GetGitHubMergeAnnouncementByIdentity
-- and the issue pull-requests read path for operator triage (review
-- condition A: Terra/C00 own exhausted-retry visibility).
UPDATE github_merge_announcement
SET status = 'failed',
    attempt_count = attempt_count + 1,
    last_error = sqlc.arg('last_error'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'pending'
RETURNING *;

-- name: SkipGitHubMergeAnnouncement :one
-- Explicit skip for a record whose delivery is not attributable right now
-- (workspace disabled, link missing, installation gone) — distinct from a
-- retriable failure so it doesn't burn attempt_count against a transient
-- fault budget. Status 'skipped' is terminal, matching 'failed'.
UPDATE github_merge_announcement
SET status = 'skipped',
    last_error = sqlc.arg('last_error'),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg('id')
  AND lease_token = sqlc.arg('lease_token')
  AND status = 'pending'
RETURNING *;

-- name: ListGitHubMergeAnnouncementsByIssue :many
-- Sanitized diagnostics surfaced through the existing issue PR read path
-- (01-RESEARCH.md's "Access Gap" section / review condition A) so an
-- operator can see terminal retry state without a new admin surface.
SELECT * FROM github_merge_announcement
WHERE issue_id = sqlc.arg('issue_id')
ORDER BY created_at DESC;
