-- CHE-1795: every claim of an outbox row gets its own token. Every statement
-- that acts for a claimant (context read, outcome, retry, drop, prune) must
-- present it, so a claimant whose lease expired while it queued cannot send or
-- overwrite the newer claim's state. NULL while the row is unclaimed.
ALTER TABLE push_delivery ADD COLUMN IF NOT EXISTS claim_token uuid;
