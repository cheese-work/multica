CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS comment_decision_answer_kind_uidx ON comment (decision_answer_id, decision_comment_kind) WHERE decision_answer_id IS NOT NULL;
