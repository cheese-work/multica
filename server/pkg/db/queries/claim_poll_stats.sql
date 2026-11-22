-- name: UpsertClaimPollStats :exec
-- CHE-8749 S7a. Adds one process's counts for a minute to the shared row; the
-- bucket arrays have the same length (the server's bucket bounds) and add by slot.
INSERT INTO claim_poll_stats (minute, path, outcome, count, sum_ms, bucket_counts)
VALUES (@minute, @path, @outcome, @count, @sum_ms, @bucket_counts::bigint[])
ON CONFLICT (minute, path, outcome) DO UPDATE
SET count = claim_poll_stats.count + EXCLUDED.count,
    sum_ms = claim_poll_stats.sum_ms + EXCLUDED.sum_ms,
    bucket_counts = (
        SELECT array_agg(claim_poll_stats.bucket_counts[i] + EXCLUDED.bucket_counts[i] ORDER BY i)
        FROM generate_series(1, cardinality(EXCLUDED.bucket_counts)) AS i
    );
