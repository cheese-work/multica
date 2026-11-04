-- DM progress cards (CHE-3546, T3a): every follow rolls up to one immutable
-- reporting root (the origin or manual follow's issue), at any depth.
ALTER TABLE dm_progress_follow ADD COLUMN reporting_root_id UUID;

-- Backfill: walk parent_follow_id to the top. A walk that loops (CYCLE) never
-- reaches a parentless follow, so the follow reports to itself.
WITH RECURSIVE walk AS (
    SELECT id AS start_id, id, issue_id, parent_follow_id FROM dm_progress_follow
    UNION ALL
    SELECT w.start_id, f.id, f.issue_id, f.parent_follow_id
    FROM walk w JOIN dm_progress_follow f ON f.id = w.parent_follow_id
) CYCLE id SET is_cycle USING path
UPDATE dm_progress_follow f
SET reporting_root_id = COALESCE(
    (SELECT w.issue_id FROM walk w WHERE w.start_id = f.id AND NOT w.is_cycle AND w.parent_follow_id IS NULL),
    f.issue_id);

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

-- L0 data incidents raised by the progress router. No FKs: swept with the workspace.
CREATE TABLE dm_progress_data_incident (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    follow_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('parent_cycle')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
