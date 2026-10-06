-- Active-run defer (CHE-1082 L8): when a fact was first held back because its
-- target was running. Null for every receipt that was never held. A defer pass
-- reads it to say why there is no run once per fact instead of every pass.
-- Additive: no default, index, foreign key or cascade.
ALTER TABLE issue_wakeup_receipt ADD COLUMN IF NOT EXISTS deferred_at timestamptz;
