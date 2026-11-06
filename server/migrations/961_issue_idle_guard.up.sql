-- The idle started-issue guard receipt (CHE-3947) moves out of
-- issue.metadata.idle_guard. issue.metadata holds only string, number and bool
-- values (the client schema rejects an object), so an issue carrying the
-- receipt object failed to open. One row per issue, one episode at a time.
CREATE TABLE issue_idle_guard (
    issue_id       uuid PRIMARY KEY REFERENCES issue(id) ON DELETE CASCADE,
    workspace_id   uuid NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    state          text NOT NULL,
    kind           text NOT NULL DEFAULT '',
    from_status    text NOT NULL DEFAULT '',
    to_status      text NOT NULL DEFAULT '',
    idle_since     timestamptz,
    moved_at       timestamptz,
    attempts       integer NOT NULL DEFAULT 0,
    alert_id       uuid,
    comment_id     uuid,
    error          text NOT NULL DEFAULT '',
    alert_attempts integer NOT NULL DEFAULT 0,
    alert_at       timestamptz,
    alert_error    text NOT NULL DEFAULT ''
);

-- The old guard wrote these fields as free text (alert_id is copied from the
-- writable decision_wait_alert_id key). A value that does not parse becomes
-- NULL or 0 instead of aborting the migration.
CREATE FUNCTION pg_temp.idle_guard_uuid(v text) RETURNS uuid LANGUAGE plpgsql AS $$
BEGIN RETURN NULLIF(v, '')::uuid; EXCEPTION WHEN others THEN RETURN NULL; END $$;
CREATE FUNCTION pg_temp.idle_guard_ts(v text) RETURNS timestamptz LANGUAGE plpgsql AS $$
BEGIN RETURN NULLIF(v, '')::timestamptz; EXCEPTION WHEN others THEN RETURN NULL; END $$;
CREATE FUNCTION pg_temp.idle_guard_int(v text) RETURNS integer LANGUAGE plpgsql AS $$
BEGIN RETURN COALESCE(NULLIF(v, '')::integer, 0); EXCEPTION WHEN others THEN RETURN 0; END $$;

INSERT INTO issue_idle_guard (issue_id, workspace_id, state, kind, from_status, to_status, idle_since, moved_at,
                              attempts, alert_id, comment_id, error, alert_attempts, alert_at, alert_error)
SELECT i.id, i.workspace_id,
       COALESCE(i.metadata -> 'idle_guard' ->> 'state', ''),
       COALESCE(i.metadata -> 'idle_guard' ->> 'kind', ''),
       COALESCE(i.metadata -> 'idle_guard' ->> 'from', ''),
       COALESCE(i.metadata -> 'idle_guard' ->> 'to', ''),
       pg_temp.idle_guard_ts(i.metadata -> 'idle_guard' ->> 'idle_since'),
       pg_temp.idle_guard_ts(i.metadata -> 'idle_guard' ->> 'moved_at'),
       pg_temp.idle_guard_int(i.metadata -> 'idle_guard' ->> 'attempts'),
       pg_temp.idle_guard_uuid(i.metadata -> 'idle_guard' ->> 'alert_id'),
       pg_temp.idle_guard_uuid(i.metadata -> 'idle_guard' ->> 'comment_id'),
       COALESCE(i.metadata -> 'idle_guard' ->> 'error', ''),
       pg_temp.idle_guard_int(i.metadata -> 'idle_guard' ->> 'alert_attempts'),
       pg_temp.idle_guard_ts(i.metadata -> 'idle_guard' ->> 'alert_at'),
       COALESCE(i.metadata -> 'idle_guard' ->> 'alert_error', '')
FROM issue i
WHERE jsonb_typeof(i.metadata -> 'idle_guard') = 'object';

-- Drop the object and add the watchdog signal in one update: the metadata
-- column is capped at 8192 bytes, so adding the signal first could overflow it.
-- The watchdog reads the status of a notified episode as a primitive.
UPDATE issue i SET metadata = CASE
    WHEN g.state = 'notified' AND g.to_status <> ''
        THEN (i.metadata - 'idle_guard') || jsonb_build_object('idle_guard_notified_to', g.to_status)
    ELSE i.metadata - 'idle_guard' END
FROM issue_idle_guard g
WHERE g.issue_id = i.id AND i.metadata ? 'idle_guard';

-- Any remaining non-object idle_guard value is dropped too.
UPDATE issue SET metadata = metadata - 'idle_guard' WHERE metadata ? 'idle_guard';

DROP FUNCTION pg_temp.idle_guard_uuid(text);
DROP FUNCTION pg_temp.idle_guard_ts(text);
DROP FUNCTION pg_temp.idle_guard_int(text);
