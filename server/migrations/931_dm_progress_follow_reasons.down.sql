ALTER TABLE dm_progress_follow
    ADD COLUMN source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('origin', 'child', 'manual')),
    ADD COLUMN parent_follow_id UUID;

UPDATE dm_progress_follow
SET source = CASE
    WHEN 'origin' = ANY(reasons) THEN 'origin'
    WHEN 'manual' = ANY(reasons) THEN 'manual'
    WHEN cardinality(reasons) > 0 THEN 'child'
    ELSE 'manual'
END;

ALTER TABLE dm_progress_follow ALTER COLUMN source DROP DEFAULT;
ALTER TABLE dm_progress_follow DROP COLUMN reasons;
