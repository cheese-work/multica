SET LOCAL lock_timeout = '2s';

DROP TRIGGER IF EXISTS agent_runtime_online_event_ins ON agent_runtime;
DROP TRIGGER IF EXISTS agent_runtime_online_event_upd ON agent_runtime;
DROP FUNCTION IF EXISTS machine_online_event_record();
