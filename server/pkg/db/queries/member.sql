-- name: ListMembers :many
SELECT * FROM member
WHERE workspace_id = $1
ORDER BY created_at ASC;

-- name: ListWorkspaceManagerUserIDs :many
SELECT user_id FROM member
WHERE workspace_id = $1 AND role IN ('owner', 'admin')
ORDER BY created_at ASC;

-- name: GetWorkspaceOwnerUserID :one
-- The longest-standing owner, so a workspace with several owners resolves to
-- the same person every time.
SELECT user_id FROM member
WHERE workspace_id = $1 AND role = 'owner'
ORDER BY created_at ASC, id ASC
LIMIT 1;

-- name: GetMember :one
SELECT * FROM member
WHERE id = $1;

-- name: GetMemberByUserAndWorkspace :one
-- A role this build does not know is no membership (CHE-9701): every access
-- check that starts from this lookup refuses it, so a later release can add a
-- role and still be rolled back to this build without it reading that role as
-- a full member.
SELECT * FROM member
WHERE user_id = $1 AND workspace_id = $2
  AND role IN ('owner', 'admin', 'member');

-- LockMemberByUserAndWorkspace holds a share lock until the caller's transaction
-- ends, so a concurrent member removal waits for it instead of slipping past.
-- name: LockMemberByUserAndWorkspace :one
SELECT * FROM member
WHERE user_id = $1 AND workspace_id = $2
FOR SHARE;

-- name: GetRealtimeMemberships :many
SELECT m.user_id, m.workspace_id
FROM member m
JOIN (
    SELECT unnest(sqlc.arg(user_ids)::uuid[]) AS user_id,
           unnest(sqlc.arg(workspace_ids)::uuid[]) AS workspace_id
) AS requested
    ON m.user_id = requested.user_id AND m.workspace_id = requested.workspace_id
-- A role this build does not know is no membership (see GetMemberByUserAndWorkspace).
WHERE m.role IN ('owner', 'admin', 'member');

-- name: CreateMember :one
INSERT INTO member (workspace_id, user_id, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: UpdateMemberRole :one
UPDATE member SET role = $2
WHERE id = $1
RETURNING *;

-- name: DeleteMember :exec
DELETE FROM member WHERE id = $1;

-- name: ListMembersWithUser :many
SELECT m.id, m.workspace_id, m.user_id, m.role, m.created_at,
       u.name as user_name, u.email as user_email, u.avatar_url as user_avatar_url
FROM member m
JOIN "user" u ON u.id = m.user_id
WHERE m.workspace_id = $1
ORDER BY m.created_at ASC;
