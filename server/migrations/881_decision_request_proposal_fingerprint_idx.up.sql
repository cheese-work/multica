CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_decision_request_open_proposal ON decision_request (workspace_id, proposal_fingerprint) WHERE status = 'open';
