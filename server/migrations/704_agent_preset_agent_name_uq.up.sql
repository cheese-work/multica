CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS agent_preset_agent_name_uq ON agent_preset (agent_id, name);
