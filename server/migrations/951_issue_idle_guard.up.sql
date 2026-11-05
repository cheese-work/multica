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

CREATE INDEX issue_idle_guard_workspace_idx ON issue_idle_guard (workspace_id);

INSERT INTO issue_idle_guard (issue_id, workspace_id, state, kind, from_status, to_status, idle_since, moved_at,
                              attempts, alert_id, comment_id, error, alert_attempts, alert_at, alert_error)
SELECT i.id, i.workspace_id,
       COALESCE(i.metadata -> 'idle_guard' ->> 'state', ''),
       COALESCE(i.metadata -> 'idle_guard' ->> 'kind', ''),
       COALESCE(i.metadata -> 'idle_guard' ->> 'from', ''),
       COALESCE(i.metadata -> 'idle_guard' ->> 'to', ''),
       NULLIF(i.metadata -> 'idle_guard' ->> 'idle_since', '')::timestamptz,
       NULLIF(i.metadata -> 'idle_guard' ->> 'moved_at', '')::timestamptz,
       COALESCE((i.metadata -> 'idle_guard' ->> 'attempts')::integer, 0),
       NULLIF(i.metadata -> 'idle_guard' ->> 'alert_id', '')::uuid,
       NULLIF(i.metadata -> 'idle_guard' ->> 'comment_id', '')::uuid,
       COALESCE(i.metadata -> 'idle_guard' ->> 'error', ''),
       COALESCE((i.metadata -> 'idle_guard' ->> 'alert_attempts')::integer, 0),
       NULLIF(i.metadata -> 'idle_guard' ->> 'alert_at', '')::timestamptz,
       COALESCE(i.metadata -> 'idle_guard' ->> 'alert_error', '')
FROM issue i
WHERE jsonb_typeof(i.metadata -> 'idle_guard') = 'object';

UPDATE issue SET metadata = metadata - 'idle_guard' WHERE metadata ? 'idle_guard';
