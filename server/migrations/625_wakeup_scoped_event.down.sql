DROP TABLE IF EXISTS wakeup_scoped_event;
ALTER TABLE issue_wakeup_definition DROP COLUMN IF EXISTS event_types;
