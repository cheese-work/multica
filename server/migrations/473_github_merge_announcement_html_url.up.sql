-- The rendered merge-announcement comment needs a link back to the PR
-- (CHE-374 review fix), but github_merge_announcement never captured one —
-- only repo_owner/repo_name/pr_number, which the comment renderer would
-- otherwise have to reconstruct into a GitHub URL by hand. Store the PR's
-- own html_url verbatim (from the webhook payload, the same field
-- github_pull_request.html_url already mirrors) instead.
--
-- Nullable: existing pending rows created before this migration have no
-- captured URL and are not backfilled — the comment renderer falls back to
-- omitting the link for those rather than fabricating one.
ALTER TABLE github_merge_announcement ADD COLUMN IF NOT EXISTS html_url TEXT;

-- The rendered comment's "remaining issue action" text needs to know whether
-- this specific merge declared closing intent (issue_pull_request.close_intent
-- at link time), so it can say something accurate instead of unconditionally
-- denying completion intent. issue_pull_request.close_intent is mutable after
-- the fact (a later webhook can flip it, or the link row can be deleted on
-- unlink), so reading it live from the delivery worker's claim time would let
-- an unrelated later edit change what a past merge's comment says. Capturing
-- it here, at announcement-creation time (same moment as merged_at), freezes
-- the value to what was true at the merge this row records.
--
-- Nullable, same reasoning as html_url: pre-migration pending rows have no
-- captured value and the renderer falls back to the pre-fix generic wording
-- for those.
ALTER TABLE github_merge_announcement ADD COLUMN IF NOT EXISTS close_intent BOOLEAN;
