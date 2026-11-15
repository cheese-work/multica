-- Not unique: the report counts duplicate event ids.
CREATE INDEX CONCURRENTLY IF NOT EXISTS system1_observation_route_event_idx ON system1_observation (workspace_id, event_id) WHERE kind = 'route';
