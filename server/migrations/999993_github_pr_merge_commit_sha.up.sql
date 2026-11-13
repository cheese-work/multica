SET LOCAL lock_timeout = '2s';

-- The merge commit of a merged PR, kept so the PR-merge handoff (CHE-5240) can
-- name it in the status history on every re-run of the decision, not only on
-- the delivery that carried it. Empty until a delivery supplies it.
ALTER TABLE github_pull_request ADD COLUMN IF NOT EXISTS merge_commit_sha TEXT NOT NULL DEFAULT '';
