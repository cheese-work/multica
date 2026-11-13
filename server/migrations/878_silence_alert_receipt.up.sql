SET LOCAL lock_timeout = '2s';

-- One row per (task, silence segment) the server has alerted on (CHE-2990).
-- The comment cannot carry this: an admin may delete a replyless system
-- comment, and the replay would then post it again. No foreign keys (repo
-- rule); task teardown deletes these rows in DeleteTaskBatch.
CREATE TABLE IF NOT EXISTS silence_alert_receipt (
    task_id uuid NOT NULL,
    segment_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (task_id, segment_id)
);
