SET LOCAL lock_timeout = '2s';

-- CHE-4279 (S7 of CHE-4265): storm breaker, coalesced digests and caps.
-- Nothing reads or writes these unless the ci_watcher feature flag is on.

ALTER TABLE ci_signature_owner
    ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'p2',
    ADD COLUMN IF NOT EXISTS fail_count integer NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_run_url text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS digest_class text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_digest_at timestamptz,
    ADD COLUMN IF NOT EXISTS check_key text NOT NULL DEFAULT '';

-- One row per breaker or cap decision and per P4 notification. `day` and the
-- partial unique index in 999c make a P4 notification once per (repo,
-- signature, day). `paused_until` is set on breaker_trip rows only.
CREATE TABLE IF NOT EXISTS ci_watch_event (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    signature_hash text NOT NULL DEFAULT '',
    kind text NOT NULL,
    run_id bigint NOT NULL DEFAULT 0,
    day date,
    paused_until timestamptz,
    detail text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
