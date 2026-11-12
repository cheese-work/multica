SET LOCAL lock_timeout = '2s';

-- Browser lease storage (CHE-4731, W3-2; plan W1 r3, F1). Additive and re-runnable.
-- No foreign keys (repo rule): workspace, agent, task, issue, user, decision, profile and
-- instance ids are soft references checked in application code. Indexes: 99961-99964, built
-- CONCURRENTLY.

-- A named browser profile and its class. S1 is deferred (mint rejects it), so no S1 row exists.
CREATE TABLE IF NOT EXISTS browser_profile (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    name text NOT NULL,
    class text NOT NULL CHECK (class IN ('S0', 'S2', 'H')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- One Steel instance. epoch is the fence: it rises with every grant and the gateway rejects a
-- stale one. phase mirrors the gateway handover state machine (Idle_clean -> Starting -> Active
-- -> Draining -> Resetting -> Verifying -> Idle_clean, or Quarantined).
CREATE TABLE IF NOT EXISTS browser_instance (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    name text NOT NULL,
    epoch bigint NOT NULL DEFAULT 0 CHECK (epoch >= 0),
    phase text NOT NULL DEFAULT 'idle_clean'
        CHECK (phase IN ('idle_clean', 'starting', 'active', 'draining', 'resetting', 'verifying', 'quarantined')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- A lease of one instance for one task. state: queued (waiting, first in first out by
-- created_at, no epoch yet), active (granted, epoch set), ended (end_reason set; release, task
-- end, revoke, expiry, ...). An ended lease still holds the instance until cleanup_acked_at is
-- set: the queue advances only after cleanup-ack for the epoch (index 99963). jti_hash is the
-- hash of the minted token id; the token itself is never stored.
CREATE TABLE IF NOT EXISTS browser_lease (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    instance_id uuid NOT NULL,
    profile_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    task_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    user_id uuid NOT NULL,
    decision_id uuid,
    class text NOT NULL CHECK (class IN ('S0', 'S2', 'H')),
    state text NOT NULL CHECK (state IN ('queued', 'active', 'ended')),
    epoch bigint CHECK (epoch > 0),
    jti_hash text,
    expires_at timestamptz NOT NULL,
    granted_at timestamptz,
    ended_at timestamptz,
    end_reason text,
    cleanup_acked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT browser_lease_epoch_iff_granted CHECK ((epoch IS NOT NULL) = (state <> 'queued')),
    CONSTRAINT browser_lease_ended_iff CHECK ((state = 'ended') = (ended_at IS NOT NULL AND end_reason IS NOT NULL)),
    CONSTRAINT browser_lease_ack_after_end CHECK (cleanup_acked_at IS NULL OR state = 'ended')
);
