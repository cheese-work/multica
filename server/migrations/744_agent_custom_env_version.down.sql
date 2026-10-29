SET LOCAL lock_timeout = '2s';

ALTER TABLE agent DROP COLUMN IF EXISTS custom_env_version;
