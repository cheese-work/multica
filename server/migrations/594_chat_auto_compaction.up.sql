ALTER TABLE agent_task_queue
    ADD COLUMN last_model_request jsonb,
    ADD COLUMN chat_compaction boolean NOT NULL DEFAULT false;
