-- Follow provenance (CHE-3473 T3c, M12): a set of reasons per follow replaces the
-- single source and parent link. A reason is 'origin', 'manual', or
-- 'inherited:<issue id of the root the follow came from>'. An unfollowed row has
-- no reasons; it stays as the negative override against inheritance.
ALTER TABLE dm_progress_follow ADD COLUMN reasons TEXT[] NOT NULL DEFAULT '{}';

UPDATE dm_progress_follow f
SET reasons = CASE
    WHEN f.state = 'unfollowed' THEN '{}'::text[]
    WHEN f.source = 'child' THEN ARRAY['inherited:' || COALESCE((SELECT p.issue_id::text FROM dm_progress_follow p WHERE p.id = f.parent_follow_id), f.issue_id::text)]
    ELSE ARRAY[f.source]
END;

ALTER TABLE dm_progress_follow DROP COLUMN source, DROP COLUMN parent_follow_id;
