-- A follow-up check is the wakeup an evidence waiver leaves behind (CHE-3239).
-- It names the decision instance it checks and survives the close of its issue:
-- the check runs after release, usually when the issue is done. No foreign key;
-- the application writes it with the settlement.
ALTER TABLE issue_wakeup ADD COLUMN IF NOT EXISTS followup_instance_id uuid;
