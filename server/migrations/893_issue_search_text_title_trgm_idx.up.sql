-- Trigram GIN index so issue search can reach issue_search_text.title_folded without scanning the
-- workspace (CHE-3164). pg_trgm comes from migration 137.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_search_text_title_trgm
    ON issue_search_text USING gin (title_folded gin_trgm_ops);
