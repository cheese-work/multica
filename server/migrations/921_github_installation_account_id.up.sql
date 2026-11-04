-- Trusted numeric GitHub account id for the allowlist policy (CHE-3418).
-- Nullable: rows bound before this migration are filled by the startup
-- backfill from the App API; a null id never passes the allowlist.
ALTER TABLE github_installation ADD COLUMN IF NOT EXISTS account_id BIGINT;
