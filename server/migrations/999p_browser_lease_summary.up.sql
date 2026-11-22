SET LOCAL lock_timeout = '2s';

-- Gateway lease summary (CHE-4745, W3-16; plan W1 r3, F6). Additive and re-runnable. The gateway
-- posts one summary per ended lease; it is stored on the lease row, write-once. summary is the
-- validated contract body (counts and bytes only, never URLs or page content).
ALTER TABLE browser_lease
    ADD COLUMN IF NOT EXISTS summary jsonb,
    ADD COLUMN IF NOT EXISTS summary_at timestamptz;
