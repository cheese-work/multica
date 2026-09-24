-- CHE-685: dashboards and any future backfill audit query
-- governance_receipt by comment_id ("did we ever capture a receipt for this
-- comment, and what did it say"). Built CONCURRENTLY and in its own
-- migration file per repo convention (see AGENTS.md).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_governance_receipt_comment_id
    ON governance_receipt (comment_id);
