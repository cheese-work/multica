SET LOCAL lock_timeout = '2s';

-- CHE-4277 (S5 of CHE-4265): columns the action ledger and scheduler need.
-- Nothing reads or writes them unless the ci_watcher feature flag is on.
-- Named 999a so it sorts after 993_ci_watch_tables (migrations apply in
-- lexical order, and 4-digit numbers would sort before 993).

ALTER TABLE ci_watch_action
    ADD COLUMN IF NOT EXISTS protocol text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS signature_hash text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS due_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS run_completed_at timestamptz NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS lease_until timestamptz,
    ADD COLUMN IF NOT EXISTS attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS reconcile_attempts integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS expected_attempt integer,
    ADD COLUMN IF NOT EXISTS post_started_at timestamptz,
    ADD COLUMN IF NOT EXISTS destination_id text,
    ADD COLUMN IF NOT EXISTS reason text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS diagnostic text NOT NULL DEFAULT '';

-- creating = claimed, issue not committed yet; open = owns an issue;
-- closed = the issue was closed (reopen rule is S7).
ALTER TABLE ci_signature_owner
    ADD COLUMN IF NOT EXISTS state text NOT NULL DEFAULT 'creating'
        CHECK (state IN ('creating', 'open', 'closed')),
    ADD COLUMN IF NOT EXISTS closed_at timestamptz;
