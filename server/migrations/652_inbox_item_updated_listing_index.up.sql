-- Back the active inbox listing, which now sorts on COALESCE(updated_at,
-- created_at). 200's created_at index stays: the archived listing still uses it.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_item_active_updated ON inbox_item (workspace_id, recipient_type, recipient_id, archived, (COALESCE(updated_at, created_at)) DESC);
