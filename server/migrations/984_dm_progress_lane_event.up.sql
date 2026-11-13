-- One-line lane events (CHE-3548): an issue a DM follows was reparented. The
-- origin lane keeps its follow and learns where the issue went; the new
-- parent's lane hears nothing unless someone follows explicitly.
-- No FKs: issue delete, chat session delete and workspace delete sweep this
-- table in application code. The (lineage_id, id) key serves the lane sweep.
CREATE TABLE dm_progress_lane_event (
    lineage_id UUID NOT NULL,
    id UUID NOT NULL DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('moved_under')),
    related_issue_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (lineage_id, id)
);
