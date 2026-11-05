-- Trigram GIN index so issue search can reach comment_search_text.content_folded without scanning the
-- workspace (CHE-3164). pg_trgm comes from migration 137.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_comment_search_text_content_trgm
    ON comment_search_text USING gin (content_folded gin_trgm_ops);
