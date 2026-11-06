SET LOCAL lock_timeout = '2s';

-- CHE-4273 (S1 of CHE-4265): storage for the CI failure watcher. Nothing reads
-- or writes these tables unless the ci_watcher feature flag is on.
--
-- No foreign keys or cascades. repo_owner and repo_name are stored lowercased.
-- Unique and lookup indexes are built CONCURRENTLY in 994-999 (repo rule: one
-- concurrent index per migration file, none inside the table migration).

-- Per workspace repo settings, every field of the settings panel.
-- branches empty = the repo's default branch. fix_owner_* names the squad or
-- agent that owns CI fixes. notify_member_id is the person told about CI work.
CREATE TABLE IF NOT EXISTS ci_watch_config (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    mode text NOT NULL DEFAULT 'off' CHECK (mode IN ('off', 'shadow', 'act')),
    branches text[] NOT NULL DEFAULT '{}',
    events text[] NOT NULL DEFAULT ARRAY['push', 'schedule', 'workflow_run', 'workflow_dispatch'],
    workflows_include text[] NOT NULL DEFAULT '{}',
    workflows_exclude text[] NOT NULL DEFAULT '{}',
    p1_mode text NOT NULL DEFAULT 'shadow' CHECK (p1_mode IN ('shadow', 'act')),
    p2_mode text NOT NULL DEFAULT 'shadow' CHECK (p2_mode IN ('shadow', 'act')),
    p3_mode text NOT NULL DEFAULT 'shadow' CHECK (p3_mode IN ('shadow', 'act')),
    p4_mode text NOT NULL DEFAULT 'shadow' CHECK (p4_mode IN ('shadow', 'act')),
    fix_owner_kind text CHECK (fix_owner_kind IN ('squad', 'agent')),
    fix_owner_id uuid,
    notify_member_id uuid,
    max_reruns_per_original_run integer NOT NULL DEFAULT 1 CHECK (max_reruns_per_original_run >= 0),
    max_new_issues_per_day integer NOT NULL DEFAULT 5 CHECK (max_new_issues_per_day >= 0),
    max_owner_wakes_per_hour integer NOT NULL DEFAULT 6 CHECK (max_owner_wakes_per_hour >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Who changed which config field, from what, to what, and when.
CREATE TABLE IF NOT EXISTS ci_watch_config_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    config_id uuid NOT NULL,
    actor_type text NOT NULL DEFAULT 'member',
    actor_id uuid NOT NULL,
    field text NOT NULL,
    old_value text,
    new_value text,
    changed_at timestamptz NOT NULL DEFAULT now()
);

-- One row per failed workflow run attempt. Kept 30 days, like
-- github_workflow_run. No logs or secrets are stored.
CREATE TABLE IF NOT EXISTS ci_failure_event (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    run_id bigint NOT NULL,
    run_attempt integer NOT NULL,
    workflow_name text NOT NULL DEFAULT '',
    event text NOT NULL DEFAULT '',
    head_branch text NOT NULL DEFAULT '',
    head_sha text NOT NULL DEFAULT '',
    conclusion text NOT NULL DEFAULT '',
    signature_hash text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);

-- One action per (repo, original run, kind). lease_epoch fences a stale
-- executor. ambiguous = the outcome is unknown and needs a human or a recheck.
CREATE TABLE IF NOT EXISTS ci_watch_action (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    original_run_id bigint NOT NULL,
    action_kind text NOT NULL,
    state text NOT NULL DEFAULT 'scheduled'
        CHECK (state IN ('scheduled', 'executing', 'done', 'failed', 'cancelled', 'ambiguous')),
    lease_epoch bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The issue that owns one failure signature in a repo. issue_id stays NULL
-- until the issue exists, and one issue owns at most one signature.
CREATE TABLE IF NOT EXISTS ci_signature_owner (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    signature_hash text NOT NULL,
    issue_id uuid,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- GitHub API rate budget per App installation, shared across workspaces, so
-- these two tables carry no workspace_id.
CREATE TABLE IF NOT EXISTS gh_installation_budget (
    installation_id bigint PRIMARY KEY,
    remaining integer NOT NULL,
    reset_at timestamptz,
    observed_at timestamptz NOT NULL DEFAULT now(),
    backoff_until timestamptz
);

CREATE TABLE IF NOT EXISTS gh_budget_reservation (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id bigint NOT NULL,
    cost integer NOT NULL CHECK (cost > 0),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- system1_observation.kind gains ci_event. NOT NULL stays; the new check is
-- added NOT VALID and validated after, which does not block writes.
ALTER TABLE system1_observation DROP CONSTRAINT IF EXISTS system1_observation_kind_check;
ALTER TABLE system1_observation ADD CONSTRAINT system1_observation_kind_check
    CHECK (kind IN ('decision', 'routing', 'ci_event')) NOT VALID;
ALTER TABLE system1_observation VALIDATE CONSTRAINT system1_observation_kind_check;
