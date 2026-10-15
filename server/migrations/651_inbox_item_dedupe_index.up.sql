CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_inbox_item_dedupe_unread ON inbox_item (recipient_id, dedupe_key) WHERE dedupe_key IS NOT NULL AND read = false;
