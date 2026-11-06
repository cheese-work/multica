-- DM progress cards (CHE-3546, T3a): every follow rolls up to one immutable
-- reporting root (the origin or manual follow's issue), at any depth.
ALTER TABLE dm_progress_follow ADD COLUMN reporting_root_id UUID;

-- L0 data incidents: a follow whose issue parent chain loops.
CREATE TABLE dm_progress_data_incident (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    follow_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('parent_cycle')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Backfill: a follow with an independent reason (origin, manual) reports to
-- itself. An inherited-only follow reports to the root its reason names; with
-- several roots the smallest id wins, so the choice is deterministic.
UPDATE dm_progress_follow f
SET reporting_root_id = COALESCE(
    (SELECT min(substr(r, length('inherited:') + 1))::uuid FROM unnest(f.reasons) AS r WHERE r LIKE 'inherited:%'),
    f.issue_id);

UPDATE dm_progress_follow f
SET reporting_root_id = f.issue_id
WHERE EXISTS (SELECT 1 FROM unnest(f.reasons) AS r WHERE r NOT LIKE 'inherited:%');

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
