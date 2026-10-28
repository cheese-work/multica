CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_passkey_enroll_open ON passkey_enroll_ceremony (user_id) WHERE consumed_at IS NULL;
