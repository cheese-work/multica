-- CHE-6987 (plan CHE-6971, scope amendment SA-1): a second item kind on the
-- CHE-2298 typed delivery table. A 'wakeup' item carries a condition wakeup's
-- instruction into a live run. event_id stays NOT NULL: for a wakeup item it is
-- a new firing id with no issue_pr_event row. No foreign keys.
SET LOCAL lock_timeout = '2s';

ALTER TABLE task_pr_event_delivery
    ADD COLUMN IF NOT EXISTS item_kind text NOT NULL DEFAULT 'pr_event',
    ADD COLUMN IF NOT EXISTS wakeup_id uuid,
    ADD COLUMN IF NOT EXISTS wakeup_revision integer,
    ADD COLUMN IF NOT EXISTS receipt_ids uuid[],
    ADD COLUMN IF NOT EXISTS note text;

-- Added NOT VALID and validated in the next file, so no long scan holds the lock.
ALTER TABLE task_pr_event_delivery
    DROP CONSTRAINT IF EXISTS task_pr_event_delivery_status_check,
    DROP CONSTRAINT IF EXISTS task_pr_event_delivery_item_kind_check,
    ADD CONSTRAINT task_pr_event_delivery_status_check CHECK (
        status IN ('pending', 'delivering', 'delivered', 'failed', 'fallback')
        AND (status <> 'fallback' OR item_kind = 'wakeup')
    ) NOT VALID,
    ADD CONSTRAINT task_pr_event_delivery_item_kind_check CHECK (
        (item_kind = 'pr_event'
            AND wakeup_id IS NULL AND wakeup_revision IS NULL
            AND receipt_ids IS NULL AND note IS NULL)
        OR (item_kind = 'wakeup'
            AND wakeup_id IS NOT NULL AND wakeup_revision IS NOT NULL
            AND cardinality(receipt_ids) > 0)
    ) NOT VALID;
