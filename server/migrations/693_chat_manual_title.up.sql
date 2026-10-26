ALTER TABLE chat_session ADD COLUMN IF NOT EXISTS title_manually_set boolean NOT NULL DEFAULT false;
