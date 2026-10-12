ALTER TABLE chat_session
    ADD COLUMN continued_from_session_id uuid,
    ADD COLUMN continued_into_session_id uuid;
