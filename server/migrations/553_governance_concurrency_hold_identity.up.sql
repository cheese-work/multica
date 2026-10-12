CREATE UNIQUE INDEX CONCURRENTLY governance_concurrency_hold_identity_uidx
ON governance_concurrency_hold (workspace_id, resource, reservation_id);
