ALTER TABLE ci_signature_owner
    DROP COLUMN IF EXISTS closed_at,
    DROP COLUMN IF EXISTS state;

ALTER TABLE ci_watch_action
    DROP COLUMN IF EXISTS diagnostic,
    DROP COLUMN IF EXISTS reason,
    DROP COLUMN IF EXISTS destination_id,
    DROP COLUMN IF EXISTS post_started_at,
    DROP COLUMN IF EXISTS expected_attempt,
    DROP COLUMN IF EXISTS reconcile_attempts,
    DROP COLUMN IF EXISTS attempts,
    DROP COLUMN IF EXISTS lease_until,
    DROP COLUMN IF EXISTS run_completed_at,
    DROP COLUMN IF EXISTS due_at,
    DROP COLUMN IF EXISTS signature_hash,
    DROP COLUMN IF EXISTS protocol;
