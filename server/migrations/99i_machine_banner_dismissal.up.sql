SET LOCAL lock_timeout = '2s';

-- A member's offline-banner dismissal, per workspace, member and machine
-- (CHE-9190). event_id is the offline period id the server produced when the
-- member closed the banner; a row counts as dismissed only while the machine's
-- current event_id equals it. Only the dismiss and reopen handlers write it,
-- and they refuse while the capability switch is off. No foreign keys (workspace
-- DB rules): workspace delete and member revoke clean up in application code.
-- The primary key covers every read and delete, so there is no secondary index.
CREATE TABLE IF NOT EXISTS machine_banner_dismissal (
    workspace_id uuid NOT NULL,
    user_id uuid NOT NULL,
    machine_owner_id uuid NOT NULL,
    daemon_id text NOT NULL,
    event_id text NOT NULL,
    dismissed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id, machine_owner_id, daemon_id)
);
