ALTER TABLE provider_breaker
    DROP COLUMN IF EXISTS canary_cool_off_s,
    DROP COLUMN IF EXISTS next_canary_at,
    DROP COLUMN IF EXISTS canary_starts,
    DROP COLUMN IF EXISTS last_release_at,
    DROP COLUMN IF EXISTS drain_cutoff_at;
