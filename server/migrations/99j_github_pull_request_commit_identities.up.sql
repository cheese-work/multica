-- CHE-9319: the identities named on a PR's commits at its snapshot head, for the
-- review-independence rule. The refresher fills it when the head changes and the
-- workspace has the review_independence setting; the merge gate reads it, so the
-- gate never calls GitHub. Shape: {"head": "<sha>", "commits": [{"sha", "author",
-- "committer", "message"}]}, where message keeps only Co-authored-by trailers.
ALTER TABLE github_pull_request ADD COLUMN IF NOT EXISTS commit_identities JSONB;
