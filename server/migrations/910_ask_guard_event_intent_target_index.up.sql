CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_ask_guard_event_intent_target
    ON ask_guard_event (intent_id, member_id) WHERE intent_id IS NOT NULL;
