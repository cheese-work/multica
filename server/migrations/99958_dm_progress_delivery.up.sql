-- One row per progress card the dispatcher sent (CHE-3369 I1.3). The primary
-- key makes a retried delivery of the same (lineage, seq) fail instead of
-- sending twice; the typed payload is the only place card content lives (I6),
-- the chat message carries a titleless fallback line.
-- No FKs: chat session delete and workspace delete sweep this table in
-- application code. Workspace delete scans it; the table holds one small row
-- per card, so no workspace index.
CREATE TABLE dm_progress_delivery (
    lineage_id UUID NOT NULL,
    seq BIGINT NOT NULL,
    workspace_id UUID NOT NULL,
    chat_message_id UUID NOT NULL,
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (lineage_id, seq)
);
