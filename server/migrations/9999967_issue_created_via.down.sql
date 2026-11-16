SET LOCAL lock_timeout = '2s';

ALTER TABLE issue DROP COLUMN IF EXISTS created_via;
