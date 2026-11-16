-- CHE-4937: one row per verified deploy (repo + deployed SHA + image digest).
-- The primary key is the dedup identity: a redelivery or a rerun of the same
-- deploy conflicts on it and creates no second release issue.
CREATE TABLE github_release_deploy (
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    repo_owner   TEXT NOT NULL,
    repo_name    TEXT NOT NULL,
    source_sha   TEXT NOT NULL,
    image_digest TEXT NOT NULL,
    old_sha      TEXT NOT NULL,
    run_id       BIGINT NOT NULL,
    issue_id     UUID REFERENCES issue(id) ON DELETE CASCADE,
    labeled_at   TIMESTAMPTZ,
    claimed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, repo_owner, repo_name, source_sha, image_digest)
);

-- A receipt can arrive before the workflow_run delivery that names its workflow
-- file. It waits here, one row per run, until that run is verified or refuted.
CREATE TABLE github_release_pending (
    workspace_id UUID NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    repo_owner   TEXT NOT NULL,
    repo_name    TEXT NOT NULL,
    run_id       BIGINT NOT NULL,
    old_sha      TEXT NOT NULL,
    source_sha   TEXT NOT NULL,
    image_digest TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, repo_owner, repo_name, run_id)
);

-- The verified delivery, stored before any installation or workspace lookup.
-- GitHub does not redeliver a failed webhook, so a lookup that fails after this
-- row is written is retried by the sweep. The row is dropped once the delivery
-- fanned out to every bound workspace; the per-workspace rows above then carry
-- any receipt that still waits for its run.
CREATE TABLE github_release_intake (
    installation_id BIGINT NOT NULL,
    repo_owner      TEXT NOT NULL,
    repo_name       TEXT NOT NULL,
    run_id          BIGINT NOT NULL,
    old_sha         TEXT NOT NULL,
    source_sha      TEXT NOT NULL,
    image_digest    TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, repo_owner, repo_name, run_id)
);
