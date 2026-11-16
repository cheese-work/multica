-- CHE-1741 M3b: release-controller state. R2 canary pacing (cool-off, next
-- canary, start log for the hourly cap), R1 stagger clock and the drain cutoff.
ALTER TABLE provider_breaker
    ADD COLUMN canary_cool_off_s integer NOT NULL DEFAULT 300,
    ADD COLUMN next_canary_at timestamptz,
    ADD COLUMN canary_starts timestamptz[] NOT NULL DEFAULT '{}',
    ADD COLUMN last_release_at timestamptz,
    ADD COLUMN drain_cutoff_at timestamptz;
