-- CHE-2147 D1a: GitHub Actions workflow jobs, kept only to track environment
-- approval gates ("waiting"). One row per (workspace, repo, job_id); job IDs are
-- unique per attempt. phase only moves forward: 1 waiting, 2 in_progress,
-- 3 completed. The indexes are built CONCURRENTLY in 716-718.
CREATE TABLE IF NOT EXISTS github_workflow_job (
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    repo_owner TEXT NOT NULL,
    repo_name TEXT NOT NULL,
    run_id BIGINT NOT NULL,
    run_attempt INTEGER NOT NULL,
    job_id BIGINT NOT NULL,
    job_name TEXT NOT NULL DEFAULT '',
    phase INTEGER NOT NULL CHECK (phase BETWEEN 1 AND 3),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
