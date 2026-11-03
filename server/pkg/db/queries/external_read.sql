-- name: MarkTaskExternalRead :exec
UPDATE agent_task_queue SET external_read = true WHERE id = $1 AND NOT external_read;

-- name: MarkIssueDescriptionExternalRead :exec
UPDATE issue SET description_external_read = true WHERE id = $1 AND NOT description_external_read;
