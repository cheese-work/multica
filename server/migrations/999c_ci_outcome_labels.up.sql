SET LOCAL lock_timeout = '2s';

-- CHE-4280 (S8 of CHE-4265): outcome labels on ci_event observations, and the
-- flake counter. outcome stays for decisions; a ci_event label lives in
-- outcome_label (rerun_green, rerun_red, fixed_by_pr, self_healed,
-- human_override). reversed_at is the existing human_override marker.
ALTER TABLE system1_observation
    ADD COLUMN IF NOT EXISTS outcome_label text,
    ADD COLUMN IF NOT EXISTS time_to_green_ms bigint,
    ADD COLUMN IF NOT EXISTS labeled_at timestamptz;

-- One row per flake increment. The unique key makes a redelivered event a
-- no-op: ref is the run id (green_rerun) or the head sha (sha_flips).
CREATE TABLE IF NOT EXISTS ci_flake_increment (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    signature_hash text NOT NULL,
    reason text NOT NULL CHECK (reason IN ('green_rerun', 'sha_flips')),
    ref text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, repo_owner, repo_name, signature_hash, reason, ref)
);
