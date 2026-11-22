-- Run-only autopilot failures have no issue; their recovery claim is keyed by
-- the failed run and its lineage instead (CHE-4568).
ALTER TABLE recovery_claim ALTER COLUMN issue_id DROP NOT NULL;
