-- DM progress cards (CHE-3546, T3a): every follow rolls up to one immutable
-- reporting root (the origin or manual follow's issue), at any depth.
ALTER TABLE dm_progress_follow ADD COLUMN reporting_root_id UUID;

-- Backfill: walk parent_follow_id to the top. A walk that loops (CYCLE) never
-- reaches a parentless follow, so the follow reports to itself and raises a
-- parent_cycle incident.
CREATE TABLE dm_progress_data_incident (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    follow_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('parent_cycle')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TEMP TABLE dm_progress_follow_top AS
WITH RECURSIVE walk AS (
    SELECT id AS start_id, id, issue_id, parent_follow_id FROM dm_progress_follow
    UNION ALL
    SELECT w.start_id, f.id, f.issue_id, f.parent_follow_id
    FROM walk w JOIN dm_progress_follow f ON f.id = w.parent_follow_id
) CYCLE id SET is_cycle USING path
SELECT start_id, issue_id AS top_issue_id FROM walk WHERE NOT is_cycle AND parent_follow_id IS NULL;

INSERT INTO dm_progress_data_incident (workspace_id, issue_id, follow_id, kind)
SELECT f.workspace_id, f.issue_id, f.id, 'parent_cycle'
FROM dm_progress_follow f
WHERE NOT EXISTS (SELECT 1 FROM dm_progress_follow_top t WHERE t.start_id = f.id);

UPDATE dm_progress_follow f
SET reporting_root_id = COALESCE((SELECT t.top_issue_id FROM dm_progress_follow_top t WHERE t.start_id = f.id), f.issue_id);

DROP TABLE dm_progress_follow_top;

ALTER TABLE dm_progress_follow ALTER COLUMN reporting_root_id SET NOT NULL;

CREATE FUNCTION dm_progress_follow_root_immutable() RETURNS trigger AS $$
BEGIN
    IF NEW.reporting_root_id IS DISTINCT FROM OLD.reporting_root_id THEN
        RAISE EXCEPTION 'dm_progress_follow.reporting_root_id is immutable';
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE TRIGGER dm_progress_follow_root_immutable
    BEFORE UPDATE ON dm_progress_follow
    FOR EACH ROW EXECUTE FUNCTION dm_progress_follow_root_immutable();
