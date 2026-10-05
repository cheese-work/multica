ALTER TABLE issue_wakeup
 DROP CONSTRAINT IF EXISTS issue_wakeup_aggregate_blocked_check,
 DROP COLUMN IF EXISTS aggregate_retry_at,
 DROP COLUMN IF EXISTS aggregate_blocked_scope_id,
 DROP COLUMN IF EXISTS aggregate_blocked_scope_kind;
DROP TABLE IF EXISTS wakeup_aggregate_reservation;
DROP TABLE IF EXISTS wakeup_aggregate_budget;
