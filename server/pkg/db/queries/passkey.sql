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

-- name: ListLivePasskeys :many
SELECT * FROM user_passkey
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY created_at, id;

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

-- True when the code belongs to an older, consumed ceremony of the same user and
-- purpose: the user typed an earlier email's code.
-- name: PasskeyEnrollCodeIsSuperseded :one
SELECT EXISTS (
    SELECT 1 FROM passkey_enroll_ceremony
    WHERE user_id = $1 AND purpose = $2 AND code_hmac = $3 AND id <> $4
      AND consumed_at IS NOT NULL AND created_at > now() - interval '1 hour'
)::bool;

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

-- Decision passkey gate (CHE-2798 P6).

-- name: LockWorkspaceDecisionGate :one
SELECT id, decision_passkey_gate, policy_revision FROM workspace WHERE id = $1 FOR UPDATE;

-- name: SetWorkspaceDecisionGate :one
UPDATE workspace SET decision_passkey_gate = $2, policy_revision = policy_revision + 1, updated_at = now()
WHERE id = $1
RETURNING decision_passkey_gate, policy_revision;

-- The person's row is the stable per-user lock that orders gate checks against
-- membership additions: a check holds it FOR SHARE (NOWAIT, so a join in flight
-- answers busy), a join holds it FOR NO KEY UPDATE until it commits. Without it
-- a join could add a blocking workspace the check's membership read never saw.

-- name: LockUserForGateCheck :exec
SELECT id FROM "user" WHERE id = $1 FOR SHARE NOWAIT;

-- name: LockUserForMembershipChange :exec
SELECT id FROM "user" WHERE id = $1 FOR NO KEY UPDATE;

-- A key share on every workspace the person belongs to: a gate flip, which
-- takes the row FOR UPDATE, cannot commit between this read and the caller's
-- commit. NOWAIT, so a flip in flight answers busy and nothing waits.

-- name: LockUserWorkspaceGates :many
SELECT w.decision_passkey_gate::text FROM member m JOIN workspace w ON w.id = m.workspace_id
WHERE m.user_id = $1 ORDER BY w.id FOR KEY SHARE OF w NOWAIT;

-- Keys that exist, suspect or not. A suspect key cannot sign, but it still
-- means the person has a passkey the gate must hear from.

-- name: CountLivePasskeys :one
SELECT count(*)::int FROM user_passkey WHERE user_id = $1 AND revoked_at IS NULL;

-- Recovery (CHE-2798 P6, B3). The C00 command takes the person's row FOR
-- UPDATE; a gate check holds it FOR SHARE, so a check in flight finishes first
-- and a later one sees recovery_required.

-- name: LockUserForRecovery :one
SELECT id FROM "user" WHERE email = $1 FOR UPDATE;

-- name: EnterPasskeyRecovery :one
UPDATE "user" SET passkey_state = 'recovery_required', session_generation = session_generation + 1, updated_at = now()
WHERE id = $1
RETURNING session_generation;

-- Suspect keys included: every credential of the person dies.

-- name: RevokeAllPasskeys :execrows
UPDATE user_passkey SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: SupersedeOpenPasskeyChallenges :exec
UPDATE passkey_assertion_challenge SET superseded_at = now()
WHERE user_id = $1 AND consumed_at IS NULL AND superseded_at IS NULL;

-- Approvals read the state under the same share lock the gate check takes.
-- NOWAIT: a recovery in flight answers busy and nothing waits.

-- name: LockUserPasskeyState :one
SELECT passkey_state FROM "user" WHERE id = $1 FOR SHARE NOWAIT;

-- Serializes a credential insert with recovery: recovery holds the row FOR
-- UPDATE, so this waits for it and then reads the bumped generation.

-- name: LockUserSessionGeneration :one
SELECT session_generation FROM "user" WHERE id = $1 FOR SHARE;

-- Ends recovery in the transaction that brings the usable keys to two.

-- name: LeavePasskeyRecovery :exec
UPDATE "user" SET passkey_state = 'ready', updated_at = now()
WHERE "user".id = $1 AND "user".passkey_state = 'recovery_required'
  AND (SELECT count(*) FROM user_passkey WHERE user_passkey.user_id = $1 AND user_passkey.revoked_at IS NULL AND user_passkey.suspect_at IS NULL) >= 2;
