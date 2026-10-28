ALTER TABLE passkey_enroll_ceremony
    ADD COLUMN purpose TEXT NOT NULL DEFAULT 'enroll' CHECK (purpose IN ('enroll', 'revoke')),
    ADD COLUMN target_passkey_id UUID;
