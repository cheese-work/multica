CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS comment_decision_request_kind_uidx ON comment (decision_request_id, decision_comment_kind) WHERE decision_answer_id IS NULL;
