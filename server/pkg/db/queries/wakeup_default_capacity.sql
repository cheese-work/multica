-- name: EnableDefaultWakeupInstance :execrows
-- Applies a default-derived instance. The capacity guard may refuse it.
UPDATE issue_wakeup SET enabled=true,disabled_at=NULL,capacity_reason=NULL,
 last_error=CASE WHEN capacity_reason IS NOT NULL THEN NULL ELSE last_error END,
 updated_at=clock_timestamp()
WHERE id= @id AND default_rule_key IS NOT NULL AND system_rule IS NULL;
-- name: MarkDefaultWakeupCapacityReached :exec
UPDATE issue_wakeup SET enabled=false,capacity_reason='default',last_error= @reason,updated_at=clock_timestamp()
WHERE id= @id AND default_rule_key IS NOT NULL AND system_rule IS NULL;
