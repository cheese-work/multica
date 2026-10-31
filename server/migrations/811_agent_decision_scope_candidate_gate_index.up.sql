CREATE UNIQUE INDEX CONCURRENTLY agent_decision_scope_candidate_gate_uidx ON decision_agent_scope (workspace_id, issue_id, candidate_sha256, waived_gate);
