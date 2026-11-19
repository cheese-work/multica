SET LOCAL lock_timeout = '2s';

-- The branch a PR targets and the repository's default branch, as the webhook
-- delivery that carried them stated them (CHE-6970). Auto-close reads them to
-- tell a PR merged into the default branch from a stacked layer. Empty until a
-- delivery supplies them, and an empty value never counts as the default branch.
ALTER TABLE github_pull_request ADD COLUMN IF NOT EXISTS base_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE github_pull_request ADD COLUMN IF NOT EXISTS default_branch TEXT NOT NULL DEFAULT '';
