CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_ask_guard_event_operation_key
    ON ask_guard_event (task_id, operation_kind, payload_sha256, mode);
