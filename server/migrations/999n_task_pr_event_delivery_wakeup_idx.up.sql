-- CHE-6987: find the open items of one wakeup (wakeup items only).
CREATE INDEX CONCURRENTLY IF NOT EXISTS task_pr_event_delivery_wakeup_idx
    ON task_pr_event_delivery (wakeup_id)
    WHERE item_kind = 'wakeup';
