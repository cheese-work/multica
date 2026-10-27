CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS agent_preset_one_default_uq ON agent_preset (agent_id) WHERE is_default;
