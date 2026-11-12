-- name: GetMemberSetting :one
SELECT * FROM member_setting
WHERE workspace_id = $1 AND user_id = $2 AND key = $3;
