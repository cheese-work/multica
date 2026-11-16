-- Proposed additive table (spec only; created here in the bench DB to measure the read path).
CREATE TABLE thread_participant (
  workspace_id     uuid        NOT NULL,
  member_id        uuid        NOT NULL,
  root_comment_id  uuid        NOT NULL,
  issue_id         uuid        NOT NULL,
  wrote_at         timestamptz,                 -- member wrote in the thread
  tagged_at        timestamptz,                 -- member tagged in the thread (notify_only tags are not stored)
  opened_at        timestamptz,                 -- member opened the thread in the frame
  joined           boolean     NOT NULL DEFAULT false, -- wrote, opened, or tagged and a non-tagger replied after the tag
  last_activity_at timestamptz NOT NULL,        -- last comment in the thread (kept by the comment write path)
  waiting          boolean     NOT NULL DEFAULT false, -- tagged without notify_only, no later write or open
  PRIMARY KEY (workspace_id, member_id, root_comment_id)
);
CREATE INDEX CONCURRENTLY thread_participant_list_idx
  ON thread_participant (workspace_id, member_id, last_activity_at DESC) WHERE joined;
CREATE INDEX CONCURRENTLY thread_participant_root_idx ON thread_participant (root_comment_id);
