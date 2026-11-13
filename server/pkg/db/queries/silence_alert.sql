-- name: LockActiveTaskForSilenceAlert :one
-- FOR SHARE conflicts with the UPDATE every terminal transition runs, so the
-- alert either commits while the task is still active or sees it finished.
SELECT id FROM agent_task_queue
WHERE id = $1 AND status IN ('dispatched', 'waiting_local_directory', 'running')
FOR SHARE;

-- name: ClaimSilenceAlertReceipt :execrows
INSERT INTO silence_alert_receipt (task_id, segment_id)
VALUES ($1, $2)
ON CONFLICT (task_id, segment_id) DO NOTHING;
