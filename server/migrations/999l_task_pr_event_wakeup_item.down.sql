-- Wakeup items and the v2 capability exist only after the up migration; code
-- before it knows only v1 PR items.
SET LOCAL lock_timeout = '2s';

DELETE FROM task_pr_event_delivery WHERE item_kind = 'wakeup';
UPDATE task_pr_event_capability
SET capability = 'task-supplement-pr-event-v1'
WHERE capability = 'task-supplement-pr-event-v2';

ALTER TABLE task_pr_event_delivery
    DROP CONSTRAINT IF EXISTS task_pr_event_delivery_status_check,
    DROP CONSTRAINT IF EXISTS task_pr_event_delivery_item_kind_check;
ALTER TABLE task_pr_event_delivery
    ADD CONSTRAINT task_pr_event_delivery_status_check
        CHECK (status IN ('pending', 'delivering', 'delivered', 'failed')) NOT VALID;
ALTER TABLE task_pr_event_delivery
    DROP COLUMN IF EXISTS item_kind,
    DROP COLUMN IF EXISTS wakeup_id,
    DROP COLUMN IF EXISTS wakeup_revision,
    DROP COLUMN IF EXISTS receipt_ids,
    DROP COLUMN IF EXISTS note;
ALTER TABLE task_pr_event_delivery VALIDATE CONSTRAINT task_pr_event_delivery_status_check;
