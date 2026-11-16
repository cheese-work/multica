SET LOCAL lock_timeout = '2s';

DROP TABLE IF EXISTS browser_gateway_credential;
ALTER TABLE browser_instance DROP CONSTRAINT IF EXISTS browser_instance_host_class_matrix;
ALTER TABLE browser_instance
    DROP COLUMN IF EXISTS allowed_classes,
    DROP COLUMN IF EXISTS gateway,
    DROP COLUMN IF EXISTS host;
