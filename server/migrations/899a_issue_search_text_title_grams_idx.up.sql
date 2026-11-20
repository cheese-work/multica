-- CHE-7422: GIN over the 1- and 2-character substrings of the folded title (see
-- title_grams in 899). Single statement: CREATE INDEX CONCURRENTLY cannot share a
-- transaction with another command. fastupdate = off: a bulk write leaves no pending
-- list for the next search to scan (with the default, a fresh 3,200-title table
-- answered a no-match query in 350 ms; without the list it took 8 ms).
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_search_text_title_grams
    ON issue_search_text USING gin (title_grams(title_folded)) WITH (fastupdate = off);
