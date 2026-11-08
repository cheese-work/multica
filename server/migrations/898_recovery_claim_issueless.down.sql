DELETE FROM recovery_claim WHERE issue_id IS NULL;
ALTER TABLE recovery_claim ALTER COLUMN issue_id SET NOT NULL;
