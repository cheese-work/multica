-- CHE-4273: CI failure watcher storage. Nothing calls these while the
-- ci_watcher feature flag is off.

-- name: DeleteExpiredCIFailureEvents :execrows
DELETE FROM ci_failure_event WHERE created_at < $1;
