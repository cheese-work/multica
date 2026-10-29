CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_user_passkey_credential ON user_passkey (credential_id);
