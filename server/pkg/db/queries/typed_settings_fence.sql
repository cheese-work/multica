-- name: LockTypedSettingsFenceShared :exec
-- Held by a typed send until its transaction ends, so a settings write that
-- turns decision_server_actions off cannot commit between the send's read of
-- the setting and its own commit. The send takes it as its first lock.
SELECT pg_advisory_xact_lock_shared(hashtextextended(sqlc.arg(workspace_id)::uuid::text || ':typed_settings_fence', 0));

-- name: TryLockTypedSettingsFence :one
-- The passkey approval only tries: it never waits for a send, so it can hold
-- row locks without joining a wait cycle. False means a send holds the fence.
SELECT pg_try_advisory_xact_lock(hashtextextended(sqlc.arg(workspace_id)::uuid::text || ':typed_settings_fence', 0));

-- name: LockTypedSettingsFence :exec
-- The owner's direct write waits for in-flight sends and blocks new ones until
-- it commits. It takes this before any other lock, so a waiter holds nothing.
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(workspace_id)::uuid::text || ':typed_settings_fence', 0));
