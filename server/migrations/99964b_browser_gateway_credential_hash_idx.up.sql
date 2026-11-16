CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS browser_gateway_credential_hash_uidx
    ON browser_gateway_credential (credential_hash);
