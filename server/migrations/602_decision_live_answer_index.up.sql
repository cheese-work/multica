CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS decision_answer_live_uidx ON decision_answer (decision_request_id) WHERE disclaimed_at IS NULL;
