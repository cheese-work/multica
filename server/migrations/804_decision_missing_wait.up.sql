CREATE TABLE decision_missing_wait (
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    checked_at timestamptz,
    comment_id uuid,
    alerted_at timestamptz
);

-- A request ends eligibility, including when the issue remains in human wait.
CREATE VIEW decision_missing_candidate AS
SELECT i.id AS issue_id, i.workspace_id, i.project_id
FROM issue i
JOIN workspace w ON w.id = i.workspace_id
LEFT JOIN issue_status s ON s.workspace_id = i.workspace_id AND s.key = i.status
WHERE COALESCE(s.category, i.status) NOT IN ('done', 'closed', 'cancelled')
  AND (i.status = 'need_decision' OR EXISTS (
      SELECT 1 FROM issue_property p,
          LATERAL jsonb_array_elements(COALESCE(p.config->'options', '[]'::jsonb)) opt
      WHERE p.workspace_id = i.workspace_id AND p.name = 'Readiness'
        AND p.type = 'select' AND p.archived_at IS NULL
        AND opt->>'name' = 'gated-human'
        AND i.properties->>p.id::text = opt->>'id'
  ))
  AND NOT EXISTS (SELECT 1 FROM decision_request r
      WHERE r.workspace_id = i.workspace_id AND r.issue_id = i.id AND r.status = 'open');
