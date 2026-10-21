-- A forked DM copy lists the ids of every message it descends from (nearest
-- last), so a decision quote card can be found for the copy. Only a lookup key:
-- the decision's own source pointers are never rewritten. Empty on a message
-- that is not a copy.
ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS lineage_message_ids uuid[] NOT NULL DEFAULT '{}';
