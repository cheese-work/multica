SET LOCAL lock_timeout = '2s';

-- CHE-4278 (S6 of CHE-4265): how the one rerun POST was answered, kept apart
-- from reason so reconcile cannot overwrite it. Empty until a POST answered.
ALTER TABLE ci_watch_action
    ADD COLUMN IF NOT EXISTS post_outcome text NOT NULL DEFAULT '';

-- When RerunFollowup routed the row (Shadow switch, P2/P4 action). NULL until then.
ALTER TABLE ci_watch_action
    ADD COLUMN IF NOT EXISTS followup_at timestamptz;
