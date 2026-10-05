-- Scoped-event outbox (CHE-1082 L9). Transactional capture of an ordinary issue
-- event for a scoped wakeup definition that has no runtime instance yet, so the
-- first matching event is not lost. A row is written in the source event's own
-- transaction, only when a definition selecting that event type exists in the
-- issue's workspace, current project or issue scope, and holds references only:
-- event, actor and source-run ids, issue and project identity, changed-field
-- names, capture time. Never a comment body, attachment URL or event archive.
-- The scheduler drains it into the existing wakeup receipts and stamps the row
-- handled with an outcome; handled and expired rows are pruned in bounded
-- batches. Identity is the unique index added by 593, the drain scan is 594 and
-- the retention scan is 595. No primary key, foreign key or cascade.
-- event_types on a definition lists the event types its trigger selects, in a
-- column so the capture trigger can early-exit on an indexed lookup. Empty for
-- every definition that exists today.
ALTER TABLE issue_wakeup_definition ADD COLUMN IF NOT EXISTS event_types text[] NOT NULL DEFAULT '{}';
CREATE TABLE IF NOT EXISTS wakeup_scoped_event (
 id uuid NOT NULL DEFAULT gen_random_uuid(),
 workspace_id uuid NOT NULL,
 issue_id uuid NOT NULL,
 project_id uuid,
 event_type text NOT NULL,
 event_key text NOT NULL,
 agent_id uuid,
 source_task_id uuid,
 actor_type text,
 actor_id text,
 payload jsonb NOT NULL,
 captured_at timestamptz NOT NULL,
 retry_at timestamptz,
 handled_at timestamptz,
 outcome text
);
