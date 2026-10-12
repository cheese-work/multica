CREATE INDEX CONCURRENTLY IF NOT EXISTS decision_answer_request_idx ON decision_answer (decision_request_id, created_at, id);
