-- Per-project GitHub routing grant (CHE-2126 plan 5.8). Default empty = deny.
-- Written only by a workspace owner/admin (handler gate); read by can_route.
-- No foreign keys (repo rule): DeleteProject removes a project's grants in its
-- transaction; a grant for a removed member or user is inert because can_route
-- re-checks membership.
CREATE TABLE github_route_grant (
    project_id UUID NOT NULL,
    user_id    UUID NOT NULL,
    granted_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, user_id)
);
