-- CHE-2147 D1a: GitHub Actions workflow runs observed through webhooks, for the
-- `github_run` wakeup condition. One row per (workspace, repo, run_id); the
-- current attempt wins, so a re-run replaces the previous attempt's state.
--
-- status_rank orders lifecycle: -1 stub (created by a job event before any run
-- event), 0 requested/queued, 1 in_progress, 2 completed. gate_generation
-- counts "waiting" gates opened for the current attempt and resets when the
-- attempt rises. repo_owner and repo_name are stored lowercased.
--
-- No raw payload, logs or secrets are stored. The identity UNIQUE index and the
-- lookup indexes are built CONCURRENTLY in 712-714 (repo rule: no index,
-- including on a new table, is built inside the table migration).
CREATE TABLE IF NOT EXISTS github_workflow_run (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    installation_id BIGINT NOT NULL,
    repo_owner TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    run_id BIGINT NOT NULL,
    run_attempt INTEGER NOT NULL,
    status_rank INTEGER NOT NULL DEFAULT -1 CHECK (status_rank BETWEEN -1 AND 2),
    status TEXT NOT NULL DEFAULT '',
    conclusion TEXT NOT NULL DEFAULT '',
    gh_updated_at TIMESTAMPTZ,
    gate_generation INTEGER NOT NULL DEFAULT 0,
    workflow_path TEXT NOT NULL DEFAULT '',
    workflow_name TEXT NOT NULL DEFAULT '',
    display_title TEXT NOT NULL DEFAULT '',
    head_sha TEXT NOT NULL DEFAULT '',
    head_branch TEXT NOT NULL DEFAULT '',
    event TEXT NOT NULL DEFAULT '',
    html_url TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
