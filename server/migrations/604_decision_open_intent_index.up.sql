CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS decision_intent_open_task_uidx ON decision_intent (task_id) WHERE consumed_at IS NULL AND closed_at IS NULL;
