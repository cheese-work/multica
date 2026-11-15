UPDATE issue i SET metadata = jsonb_set(i.metadata, '{idle_guard}', jsonb_strip_nulls(jsonb_build_object(
    'state', g.state,
    'kind', g.kind,
    'from', g.from_status,
    'to', g.to_status,
    'idle_since', COALESCE(to_char(g.idle_since AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), ''),
    'moved_at', COALESCE(to_char(g.moved_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
    'attempts', g.attempts,
    'alert_id', g.alert_id,
    'comment_id', g.comment_id,
    'error', NULLIF(g.error, ''),
    'alert_attempts', NULLIF(g.alert_attempts, 0),
    'alert_at', to_char(g.alert_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'alert_error', NULLIF(g.alert_error, ''))))
FROM issue_idle_guard g WHERE g.issue_id = i.id;

UPDATE issue SET metadata = metadata - 'idle_guard_notified_to' WHERE metadata ? 'idle_guard_notified_to';

DROP TABLE issue_idle_guard;
