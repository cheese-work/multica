-- Backfill the folded search text (CHE-2943). It runs in its own transaction,
-- after migration 886 has committed and released its trigger locks, so writes
-- to issue and comment are never blocked while it folds existing rows. A row
-- written since 886 already has its side row (the sync triggers upsert it), so
-- the backfill skips those and never overwrites fresher text.
--
-- FOR KEY SHARE locks each source row while its side row is written. A delete
-- of that row waits for this transaction, and the delete trigger then removes
-- the side row. A delete already in flight makes the backfill wait, and the
-- backfill then skips the deleted row. Without the lock, a delete that commits
-- between the snapshot and the insert leaves a side row that search would keep
-- returning. Updates to title, description and content are not blocked.

INSERT INTO issue_search_text (issue_id, workspace_id, title_folded, description_folded, title_accents, description_accents)
SELECT id, workspace_id, folded_title, folded_description,
    CASE WHEN lowered_title <> folded_title THEN lowered_title END,
    CASE WHEN lowered_description <> folded_description THEN lowered_description END
FROM issue
CROSS JOIN LATERAL (SELECT lower(COALESCE(title, '')) AS lowered_title, lower(COALESCE(description, '')) AS lowered_description,
    search_fold(title) AS folded_title, search_fold(description) AS folded_description) AS t
FOR KEY SHARE OF issue
ON CONFLICT (issue_id) DO NOTHING;

INSERT INTO comment_search_text (comment_id, workspace_id, issue_id, created_at, content_folded, content_accents)
SELECT id, workspace_id, issue_id, created_at, folded, CASE WHEN lowered <> folded THEN lowered END
FROM comment
CROSS JOIN LATERAL (SELECT lower(COALESCE(content, '')) AS lowered, search_fold(content) AS folded) AS t
FOR KEY SHARE OF comment
ON CONFLICT (comment_id) DO NOTHING;
