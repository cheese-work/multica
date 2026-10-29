-- github_dispatch_halt: the kill switch for GitHub-triggered dispatch (CHE-2220).
-- A row halts PR-driven wakeups and review wakeups for its scope: '*' (the
-- whole workspace), a GitHub owner ('acme'), or one repo ('acme/widget'),
-- lower-cased. No row, no halt. Rows are written only by the owner/admin API.
CREATE TABLE github_dispatch_halt (
    workspace_id UUID NOT NULL,
    scope TEXT NOT NULL CHECK (scope = lower(scope) AND scope <> ''),
    halted_by UUID,
    halted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, scope)
);
