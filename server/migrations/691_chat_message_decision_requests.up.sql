ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS decision_request_ids uuid[] NOT NULL DEFAULT '{}';
