-- A decision_reference DM message names the decision_answer it announces, so
-- clients read the link instead of parsing the message text. Rows written
-- before this column keep NULL and render as plain messages.
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS decision_answer_id uuid;
