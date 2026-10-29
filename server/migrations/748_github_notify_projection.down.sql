DELETE FROM github_webhook_dispatch WHERE state <> 'dispatched';
ALTER TABLE comment DROP COLUMN IF EXISTS github_notify_only;
ALTER TABLE github_webhook_dispatch
    DROP CONSTRAINT IF EXISTS github_webhook_dispatch_effect_check,
    ALTER COLUMN comment_id SET NOT NULL,
    ALTER COLUMN task_id SET NOT NULL,
    DROP COLUMN IF EXISTS reason,
    DROP COLUMN IF EXISTS state,
    DROP COLUMN IF EXISTS revision_id;
ALTER TABLE github_webhook_revision DROP COLUMN IF EXISTS id;
