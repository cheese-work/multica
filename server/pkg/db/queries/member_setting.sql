-- name: GetMemberSetting :one
SELECT * FROM member_setting
WHERE workspace_id = $1 AND user_id = $2 AND key = $3;

-- name: UpsertMemberSetting :exec
INSERT INTO member_setting (workspace_id, user_id, key, value)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, user_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteMemberSetting :exec
DELETE FROM member_setting
WHERE workspace_id = $1 AND user_id = $2 AND key = $3;
