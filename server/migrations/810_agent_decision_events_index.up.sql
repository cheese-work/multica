CREATE INDEX CONCURRENTLY agent_decision_events_idx ON governed_vote_record (decision_instance_id, decision_sequence) WHERE decision_instance_id IS NOT NULL;
