-- name: GetPasskeyChallenge :one
SELECT * FROM passkey_assertion_challenge WHERE id = $1;

-- One open assertion challenge per user across every purpose. The issuer
-- locks it NOWAIT: a verification holding it makes issuing fail fast.

-- The open-row lock cannot serialize two first issuers: before the first
-- commits there is no row to lock and the second would wait on the unique
-- index. This per-user advisory lock is taken first and never waits.

-- name: TryLockPasskeyChallengeIssue :one
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg(user_id)::uuid::text || ':passkey_challenge', 0))::boolean;

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
SELECT challenge.*, (challenge.expires_at <= clock_timestamp())::bool AS expired
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

-- name: GetPasskey :one
SELECT * FROM user_passkey WHERE id = $1;

-- name: GetPasskeyIDByCredential :one
SELECT id FROM user_passkey WHERE credential_id = $1;

-- Locks in ascending id order, all NOWAIT, so two revocations that touch the
-- same pair of credentials take them in the same order.

-- name: LockPasskeysNowait :many
SELECT * FROM user_passkey WHERE id = ANY(@ids::uuid[]) ORDER BY id FOR UPDATE NOWAIT;

-- name: RevokePasskey :exec
UPDATE user_passkey SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: AdvancePasskey :exec
UPDATE user_passkey SET sign_count = $2, last_used_at = now() WHERE id = $1;

-- name: MarkPasskeySuspect :exec
UPDATE user_passkey SET suspect_at = now() WHERE id = $1 AND suspect_at IS NULL;

-- Enrollment. Every enrollment transaction takes the limit row first (NOWAIT),
-- then the ceremony, so start and finish cannot deadlock.

-- name: EnsurePasskeyEnrollLimit :exec
INSERT INTO passkey_enroll_limit (user_id) VALUES ($1) ON CONFLICT (user_id) DO NOTHING;

-- name: LockPasskeyEnrollLimitNowait :one
SELECT user_id,
       (locked_until IS NOT NULL AND locked_until > now())::bool AS locked,
       CASE WHEN now() - hour_started_at >= interval '1 hour' THEN 0 ELSE hour_starts END::int AS hour_count,
       CASE WHEN now() - day_started_at >= interval '1 day' THEN 0 ELSE day_starts END::int AS day_count
FROM passkey_enroll_limit WHERE user_id = $1 FOR UPDATE NOWAIT;

-- name: CountPasskeyEnrollStart :exec
UPDATE passkey_enroll_limit SET
    hour_starts = CASE WHEN now() - hour_started_at >= interval '1 hour' THEN 1 ELSE hour_starts + 1 END,
    hour_started_at = CASE WHEN now() - hour_started_at >= interval '1 hour' THEN now() ELSE hour_started_at END,
    day_starts = CASE WHEN now() - day_started_at >= interval '1 day' THEN 1 ELSE day_starts + 1 END,
    day_started_at = CASE WHEN now() - day_started_at >= interval '1 day' THEN now() ELSE day_started_at END
WHERE user_id = $1;

-- name: LockPasskeyEnrollment :exec
UPDATE passkey_enroll_limit SET locked_until = now() + interval '24 hours' WHERE user_id = $1;

-- name: ConsumeOpenPasskeyEnrollCeremony :exec
UPDATE passkey_enroll_ceremony SET consumed_at = now() WHERE user_id = $1 AND consumed_at IS NULL;

-- name: InsertPasskeyEnrollCeremony :one
INSERT INTO passkey_enroll_ceremony (user_id, code_hmac, session_data, purpose, target_passkey_id, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + interval '10 minutes')
RETURNING id, expires_at;

-- name: LockPasskeyEnrollCeremonyNowait :one
SELECT ceremony.*, (ceremony.expires_at <= clock_timestamp())::bool AS expired
FROM passkey_enroll_ceremony ceremony WHERE ceremony.id = $1 FOR UPDATE NOWAIT;

-- A wrong code or a rejected attestation costs one attempt. The fifth ends
-- the ceremony as failed.
-- name: BumpPasskeyEnrollAttempt :one
UPDATE passkey_enroll_ceremony SET
    attempts = attempts + 1,
    failed_at = CASE WHEN attempts + 1 >= 5 THEN now() ELSE failed_at END,
    consumed_at = CASE WHEN attempts + 1 >= 5 THEN now() ELSE consumed_at END
WHERE id = $1
RETURNING attempts;

-- name: ConsumePasskeyEnrollCeremony :exec
UPDATE passkey_enroll_ceremony SET consumed_at = now() WHERE id = $1;

-- name: CountFailedPasskeyEnrollCeremonies :one
SELECT count(*)::int FROM passkey_enroll_ceremony
WHERE user_id = $1 AND failed_at IS NOT NULL AND failed_at > now() - interval '24 hours';

-- name: InsertPasskey :one
INSERT INTO user_passkey (user_id, credential_id, public_key, attestation_type, aaguid, sign_count, transports, flags)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: InsertPasskeyEvent :exec
INSERT INTO passkey_event (user_id, action, details) VALUES ($1, $2, $3);
