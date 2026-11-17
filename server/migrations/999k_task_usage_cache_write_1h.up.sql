-- How many of cache_write_tokens Anthropic wrote with the 1-hour TTL (billed 2x
-- input instead of the 1.25x 5-minute rate). NULL means the run carried no TTL
-- split — every pre-existing row and every non-Claude provider — and is priced
-- at the 5-minute rate as before. 0 is a real "all 5-minute".
ALTER TABLE task_usage
    ADD COLUMN IF NOT EXISTS cache_write_1h_tokens BIGINT;
