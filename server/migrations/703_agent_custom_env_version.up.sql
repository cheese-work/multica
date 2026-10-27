SET LOCAL lock_timeout = '2s';

-- Opaque write counter for custom_env. Unlike envRevision (a SHA-256 of the
-- plaintext map, readable only by env managers), it leaks nothing about the
-- values, so approve-to-execute preconditions and results may carry it.
ALTER TABLE agent ADD COLUMN IF NOT EXISTS custom_env_version bigint NOT NULL DEFAULT 0;
