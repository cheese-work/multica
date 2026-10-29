CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_passkey_assertion_open ON passkey_assertion_challenge (user_id) WHERE consumed_at IS NULL AND superseded_at IS NULL;
