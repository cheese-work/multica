-- Outbox of structural issue changes a DM lane has not been told about yet
-- (CHE-3369 I1.2). Rows are written in the transaction that changes the issue
-- (triggers, migration 990) and consumed by the dispatcher.
-- event_kind: status | assignee | pr. immediate: a root follow entered
-- blocked, done or cancelled (I2.2). event_id is generated per row, so two
-- transitions of one class are two events.
-- No FKs: issue delete, chat session delete and workspace delete sweep this
-- table in application code. The primary key serves the lane scan; the table
-- holds only unsent changes, so the issue sweep scans it.
CREATE TABLE dm_progress_pending (
    lineage_id UUID NOT NULL,
    event_id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status', 'assignee', 'pr')),
    immediate BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (lineage_id, event_id)
);
