-- One request answers at most one ask guard event (CHE-3345 S3). A retry that
-- names a second event hits this index instead of clearing it.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_ask_guard_event_cleared_by_request
    ON ask_guard_event (cleared_by_request_id) WHERE cleared_by_request_id IS NOT NULL;
