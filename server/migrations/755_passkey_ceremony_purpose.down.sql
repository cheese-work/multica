ALTER TABLE passkey_enroll_ceremony
    DROP COLUMN IF EXISTS target_passkey_id,
    DROP COLUMN IF EXISTS purpose;
