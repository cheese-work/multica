-- Activation of scoped wakeup definition writes (CHE-1082 L13). One row per
-- activation (the production one is 'legacy_backfill') holds where the bounded,
-- restartable legacy backfill stands: state 'backfill' while it pages through
-- customized platform-rule rows, 'verify' while a second pass proves nothing is
-- left and nothing but backfilled issue overrides exists, 'complete' once the
-- definition-write gate may open. The cursor advances in the same transaction
-- as the page it covers, so an interrupted run resumes where it stopped.
-- last_error keeps the reason a pass could not finish. No foreign key.
CREATE TABLE IF NOT EXISTS wakeup_definition_activation (
 name text PRIMARY KEY,
 state text NOT NULL DEFAULT 'backfill',
 cursor uuid,
 scanned bigint NOT NULL DEFAULT 0,
 inserted bigint NOT NULL DEFAULT 0,
 last_error text,
 started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 completed_at timestamptz,
 CONSTRAINT wakeup_definition_activation_state_check CHECK (state IN ('backfill','verify','complete'))
);
