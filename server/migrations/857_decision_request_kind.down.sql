ALTER TABLE decision_request DROP CONSTRAINT IF EXISTS decision_request_kind_check;
ALTER TABLE decision_request DROP COLUMN IF EXISTS expires_at, DROP COLUMN IF EXISTS kind;
