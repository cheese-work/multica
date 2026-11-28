SET LOCAL lock_timeout = '2s';

-- Durable offline-period id of a machine (CHE-9254). One row per committed
-- online transition, written by the trigger of the next migration; the offline
-- banner id is offline:<owner>/<daemon>:e<generation>.<number of rows>, so it
-- changes at every committed transition and at no runtime delete.
-- machine_owner_id is COALESCE(owner_id, workspace_id) of the runtime, the same
-- key ListAffectedMachines groups by. seq comes from an identity sequence and
-- leads no uniqueness check across transactions; the key order lets the
-- per-machine count read the primary key alone. No foreign keys (workspace DB
-- rules), no prune: a future prune must also set a new generation.
CREATE TABLE IF NOT EXISTS machine_online_event (
    machine_owner_id uuid NOT NULL,
    daemon_id text NOT NULL,
    seq bigint GENERATED ALWAYS AS IDENTITY,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (machine_owner_id, daemon_id, seq)
);

-- One row. A new generation changes every machine's id, a machine with no
-- events included, so no id from before a schema reset or a trigger gap comes
-- back. Created here in the same transaction as the event table.
CREATE TABLE IF NOT EXISTS machine_online_event_generation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    generation uuid NOT NULL
);

INSERT INTO machine_online_event_generation (generation)
VALUES (gen_random_uuid())
ON CONFLICT DO NOTHING;
