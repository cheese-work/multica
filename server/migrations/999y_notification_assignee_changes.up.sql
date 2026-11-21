-- CHE-6400: `unassigned` and `assignee_changed` move out of the `assignments`
-- group into `assignee_changes`. Copy a muted `assignments` switch so nobody's
-- delivery changes. An absent key means "all", so other rows need nothing.
UPDATE notification_preference
SET preferences = preferences || '{"assignee_changes":"muted"}'
WHERE preferences->>'assignments' = 'muted'
  AND NOT preferences ? 'assignee_changes';
