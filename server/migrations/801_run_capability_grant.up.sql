SET LOCAL lock_timeout = '2s';

-- Member-written widening of one issue's runs (CHE-2216 plan 3.1). Written only
-- through the member-only handler; an agent token never reaches the insert.
-- No foreign keys (repo rule): workspace, issue and member are validated by the
-- handler. A grant is never edited: revocation sets revoked_at in a later slice.
CREATE TABLE IF NOT EXISTS run_capability_grant (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    scopes text[] NOT NULL CHECK (cardinality(scopes) BETWEEN 1 AND 16),
    granted_by_member_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL,
    reason text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    revoked_at timestamptz,
    pinned_base_sha text CHECK (pinned_base_sha IS NULL OR pinned_base_sha ~ '^[0-9a-f]{40}$'),
    pinned_head_sha text CHECK (pinned_head_sha IS NULL OR pinned_head_sha ~ '^[0-9a-f]{40}$'),
    CONSTRAINT run_capability_grant_max_24h CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours')
);
