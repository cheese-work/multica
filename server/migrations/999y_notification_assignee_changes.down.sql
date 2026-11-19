-- Removes only the copies the up migration could have made. A row that holds
-- `assignee_changes` = `all` keeps it.
UPDATE notification_preference
SET preferences = preferences - 'assignee_changes'
WHERE preferences->>'assignments' = 'muted'
  AND preferences->>'assignee_changes' = 'muted';
