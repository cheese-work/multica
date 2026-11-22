SET LOCAL lock_timeout = '2s';

-- CHE-8749 S7a: one row per minute, claim path and poll outcome. Each server
-- process keeps an in-process histogram of every claim poll and adds its
-- counts here once a minute, so several processes sum into one row. No foreign
-- keys. bucket_counts holds one slot per upper bound in the server's
-- ClaimPollBucketBoundsMs (inclusive, milliseconds) plus a final overflow slot.
CREATE TABLE IF NOT EXISTS claim_poll_stats (
    minute timestamptz NOT NULL,
    path text NOT NULL,
    outcome text NOT NULL,
    count bigint NOT NULL,
    sum_ms double precision NOT NULL,
    bucket_counts bigint[] NOT NULL,
    PRIMARY KEY (minute, path, outcome)
);
