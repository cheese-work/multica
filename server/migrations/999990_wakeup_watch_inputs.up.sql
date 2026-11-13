-- Inputs a time wakeup (at, every, cron) declares it watches (CHE-3592). This
-- column only records them; the server does not skip fires on it yet. Empty
-- means undeclared: always run.
ALTER TABLE issue_wakeup ADD COLUMN IF NOT EXISTS watch_inputs TEXT[] NOT NULL DEFAULT '{}';
