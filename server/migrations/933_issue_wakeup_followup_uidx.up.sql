CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS issue_wakeup_followup_instance_uidx ON issue_wakeup (followup_instance_id) WHERE followup_instance_id IS NOT NULL;
