SET LOCAL lock_timeout = '2s';

-- Browser permission model (CHE-4737, W3-8; plan CHE-4695 W1 r3, F3). Additive and re-runnable.
-- Default deny: a grant is a row, no row means no access. A lease needs both: the task's
-- accountable user is in browser_profile_member and the agent is in browser_profile_agent for
-- the profile (W3-9 enforces it at mint, renew and queued grant). No foreign keys (repo rule):
-- workspace, profile, user, agent and granter ids are soft references checked in application
-- code. The unique indexes are 99964d and 99964e, built CONCURRENTLY.
--
-- There is no egress, host or allow column on either table: the plan allows no egress exception.

CREATE TABLE IF NOT EXISTS browser_profile_member (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    profile_id uuid NOT NULL,
    user_id uuid NOT NULL,
    granted_by uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE IF NOT EXISTS browser_profile_agent (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    profile_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    granted_by uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
