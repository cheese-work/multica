SET LOCAL lock_timeout = '2s';

-- Site hosts of a browser profile (CHE-4782, W3-19; plan W1 r5, "Class-3 deny list", mint check).
-- Recorded once when the profile is created. Mint refuses an S2 lease for a profile whose hosts
-- intersect the host part of class3-deny.txt. Additive and re-runnable; existing rows get no
-- hosts, so an S2 profile that predates this column is refused until its hosts are recorded.
ALTER TABLE browser_profile
    ADD COLUMN IF NOT EXISTS hosts text[] NOT NULL DEFAULT ARRAY[]::text[];
