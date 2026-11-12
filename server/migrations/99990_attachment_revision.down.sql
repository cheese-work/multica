SET LOCAL lock_timeout = '2s';
ALTER TABLE attachment
    DROP COLUMN IF EXISTS sha256,
    DROP COLUMN IF EXISTS revision_root,
    DROP COLUMN IF EXISTS revision_of,
    DROP COLUMN IF EXISTS revision;
