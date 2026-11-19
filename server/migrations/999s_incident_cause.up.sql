-- One row per stop cause: (workspace, stopped issue, signal kind). While open it
-- points at the incident issue that carries the timeline (CHE-5738). No FKs:
-- issue and workspace delete sweep this table in app code.
CREATE TABLE incident_cause (
    id UUID PRIMARY KEY,
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    kind TEXT NOT NULL,
    incident_issue_id UUID NOT NULL,
    last_event_key TEXT NOT NULL DEFAULT '',
    opened_at TIMESTAMPTZ NOT NULL,
    closed_at TIMESTAMPTZ
);
