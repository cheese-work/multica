-- A forked DM copy keeps the id of the message it was copied from, so a decision
-- quote card can be found for the copy. Only a lookup key: the decision's own
-- source pointers are never rewritten. NULL on every message that is not a copy.
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS lineage_message_id uuid;
