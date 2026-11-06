-- A call that finished without usable rate headers stays on the ledger as a
-- settled reservation until a later response or a refresh accounts for it.
ALTER TABLE gh_budget_reservation ADD COLUMN IF NOT EXISTS settled_at timestamptz;
