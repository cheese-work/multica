CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS decision_wake_event_request_seq_uidx ON decision_wake_event (request_id, seq);
