CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS decision_wake_receipt_event_recipient_uidx ON decision_wake_receipt (event_id, recipient_agent_id);
