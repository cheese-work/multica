-- Browser permission model (CHE-4737, W3-8). Default deny: a grant is a row.

-- name: GrantBrowserProfileMember :exec
INSERT INTO browser_profile_member (workspace_id, profile_id, user_id, granted_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (profile_id, user_id) DO NOTHING;

-- name: RevokeBrowserProfileMember :exec
DELETE FROM browser_profile_member
WHERE workspace_id = $1 AND profile_id = $2 AND user_id = $3;

-- name: ListBrowserProfileMembers :many
SELECT user_id FROM browser_profile_member
WHERE workspace_id = $1 AND profile_id = $2
ORDER BY created_at, id;

-- name: BrowserProfileHasMember :one
SELECT EXISTS (
    SELECT 1 FROM browser_profile_member
    WHERE workspace_id = $1 AND profile_id = $2 AND user_id = $3
);

-- name: GrantBrowserProfileAgent :exec
INSERT INTO browser_profile_agent (workspace_id, profile_id, agent_id, granted_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (profile_id, agent_id) DO NOTHING;

-- name: RevokeBrowserProfileAgent :exec
DELETE FROM browser_profile_agent
WHERE workspace_id = $1 AND profile_id = $2 AND agent_id = $3;

-- name: ListBrowserProfileAgents :many
SELECT agent_id FROM browser_profile_agent
WHERE workspace_id = $1 AND profile_id = $2
ORDER BY created_at, id;

-- name: BrowserProfileHasAgent :one
SELECT EXISTS (
    SELECT 1 FROM browser_profile_agent
    WHERE workspace_id = $1 AND profile_id = $2 AND agent_id = $3
);
