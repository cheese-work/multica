-- name: GetPasskeyChallenge :one
SELECT * FROM passkey_assertion_challenge WHERE id = $1;

-- One open assertion challenge per user across every purpose. The issuer
-- locks it NOWAIT: a verification holding it makes issuing fail fast.

-- name: LockOpenPasskeyChallenge :one
SELECT * FROM passkey_assertion_challenge
WHERE user_id = $1 AND consumed_at IS NULL AND superseded_at IS NULL
FOR UPDATE NOWAIT;

-- name: SupersedePasskeyChallenge :exec
UPDATE passkey_assertion_challenge SET superseded_at = now() WHERE id = $1;

-- name: InsertPasskeyChallenge :one
INSERT INTO passkey_assertion_challenge (nonce, user_id, purpose, subject_id, session_data, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: LockPasskeyChallengeNowait :one
SELECT challenge.*, (challenge.expires_at <= now())::bool AS expired
FROM passkey_assertion_challenge challenge
WHERE challenge.id = $1
FOR UPDATE NOWAIT;

-- name: ConsumePasskeyChallenge :exec
UPDATE passkey_assertion_challenge SET consumed_at = now() WHERE id = $1;

-- name: ListUsablePasskeys :many
SELECT * FROM user_passkey
WHERE user_id = $1 AND revoked_at IS NULL AND suspect_at IS NULL
ORDER BY id;

-- name: LockPasskeyByCredentialNowait :one
SELECT * FROM user_passkey WHERE credential_id = $1 FOR UPDATE NOWAIT;

-- name: AdvancePasskey :exec
UPDATE user_passkey SET sign_count = $2, last_used_at = now() WHERE id = $1;

-- name: MarkPasskeySuspect :exec
UPDATE user_passkey SET suspect_at = now() WHERE id = $1 AND suspect_at IS NULL;
