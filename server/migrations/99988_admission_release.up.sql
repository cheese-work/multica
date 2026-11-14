-- CHE-5265 (CHE-5213 slice 1): one row per ended run that owes its project a
-- release (admit the next held run). Written in the run's terminal transaction,
-- deleted by the release that delivered it. No foreign key, no cascade: a row
-- outlives its run and is cleaned by the release of its project.
CREATE TABLE IF NOT EXISTS admission_release (
    task_id UUID PRIMARY KEY,
    project_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
