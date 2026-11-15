DROP TABLE IF EXISTS decision_wake_receipt;
DROP TABLE IF EXISTS decision_wake_event;
ALTER TABLE decision_request DROP COLUMN IF EXISTS wake_seq;
