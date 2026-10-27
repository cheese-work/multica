DROP TABLE IF EXISTS github_webhook_dispatch;
ALTER TABLE github_webhook_intake DROP COLUMN IF EXISTS attempts, DROP COLUMN IF EXISTS next_attempt_at;
