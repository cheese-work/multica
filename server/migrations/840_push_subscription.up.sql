-- CHE-1795: Web Push subscriptions. One row per browser endpoint, owned by one
-- user across all workspaces. session_id is the `sid` of the login that
-- registered it and session_valid_until that session's expiry; logout (or
-- expiry) ends it, so a device whose DELETE never arrived still stops getting
-- pushes. The unique endpoint index follows in 841, the owner index in 842.
-- No foreign keys; dependents are cleaned in application code.
CREATE TABLE IF NOT EXISTS push_subscription (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id uuid NOT NULL,
    endpoint text NOT NULL,
    p256dh text NOT NULL,
    auth text NOT NULL,
    session_id text NOT NULL,
    session_valid_until timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_success_at timestamptz
);
