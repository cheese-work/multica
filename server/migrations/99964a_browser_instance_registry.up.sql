SET LOCAL lock_timeout = '2s';

-- Browser instance registry (CHE-4781, W3-18; plan W1 r5, "Host and class matrix" and "Token
-- model"). Additive and re-runnable: W3-2's 99960 file stays as it is. No foreign keys (repo
-- rule). The unique index on the credential hash is 99964b, built CONCURRENTLY.

-- Each instance lives on one host behind that host's gateway, and serves only the classes the
-- host matrix allows: C00 serves S2 and H, X99 serves S0. No class exception: the matrix is a
-- CHECK, so no code path can register S0 on C00 or S2 or H on X99. Rows that exist before this
-- migration take the C00 defaults.
ALTER TABLE browser_instance
    ADD COLUMN IF NOT EXISTS host text NOT NULL DEFAULT 'c00',
    ADD COLUMN IF NOT EXISTS gateway text NOT NULL DEFAULT 'c00',
    ADD COLUMN IF NOT EXISTS allowed_classes text[] NOT NULL DEFAULT ARRAY['S2', 'H']::text[];

ALTER TABLE browser_instance
    DROP CONSTRAINT IF EXISTS browser_instance_host_class_matrix;
ALTER TABLE browser_instance
    ADD CONSTRAINT browser_instance_host_class_matrix CHECK (
        gateway = host
        AND cardinality(allowed_classes) > 0
        AND (
            (host = 'c00' AND allowed_classes <@ ARRAY['S2', 'H']::text[])
            OR (host = 'x99' AND allowed_classes <@ ARRAY['S0']::text[])
        )
    );

-- One credential per gateway (C00: the steel-browser plugin install token; X99: its own). Only
-- the hash is stored. A credential resolves to the instances of its own gateway and no others:
-- the join is on gateway and workspace (see GetBrowserInstanceForGateway).
CREATE TABLE IF NOT EXISTS browser_gateway_credential (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    gateway text NOT NULL CHECK (gateway IN ('c00', 'x99')),
    credential_hash text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz
);
