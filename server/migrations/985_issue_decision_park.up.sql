SET LOCAL lock_timeout = '2s';

-- The decision park is internal state, not issue metadata: metadata holds
-- primitive values only, and an object under decision_park made clients reject
-- the issue (CHE-3946). The park gets its own table, one row per parked issue.
CREATE TABLE IF NOT EXISTS issue_decision_park (
    issue_id uuid PRIMARY KEY REFERENCES issue (id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL,
    prior_status text NOT NULL,
    to_status text NOT NULL,
    readiness_id text,
    prior_readiness jsonb,
    set_value jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

INSERT INTO issue_decision_park (issue_id, workspace_id, prior_status, to_status, readiness_id, prior_readiness, set_value)
SELECT i.id, i.workspace_id,
       COALESCE(i.metadata -> 'decision_park' ->> 'status', ''),
       COALESCE(i.metadata -> 'decision_park' ->> 'to', ''),
       NULLIF(i.metadata -> 'decision_park' ->> 'readiness_id', ''),
       i.metadata -> 'decision_park' -> 'readiness',
       i.metadata -> 'decision_park' -> 'set'
FROM issue i
WHERE jsonb_typeof(i.metadata -> 'decision_park') = 'object'
ON CONFLICT (issue_id) DO NOTHING;

UPDATE issue SET metadata = metadata - 'decision_park'
WHERE jsonb_typeof(metadata -> 'decision_park') = 'object';
