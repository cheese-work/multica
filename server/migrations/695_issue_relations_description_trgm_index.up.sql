CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_description_trgm
    ON issue USING gin (LOWER(COALESCE(description, '')) gin_trgm_ops);
