-- Ask recovery notice (CHE-3345 S3/C6). One row per delivered notice: the DM
-- message, the issue comment and the inbox item commit with it, so a retry
-- finds the row and sends nothing. The event keeps the source it quotes
-- (source_text), whether a notice was delivered (delivered_at) and why it was
-- not (repair_reason, read by the repair slice). Its unique indexes are the next
-- migrations. No foreign keys (repo rule).
CREATE TABLE IF NOT EXISTS ask_recovery_notice (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    event_id uuid NOT NULL,
    member_id uuid NOT NULL,
    chat_session_id uuid NOT NULL,
    chat_message_id uuid NOT NULL,
    comment_id uuid NOT NULL,
    state text NOT NULL DEFAULT 'delivered' CHECK (state IN ('delivered', 'answered')),
    delivered_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    answered_at timestamptz
);
ALTER TABLE ask_guard_event
    ADD COLUMN IF NOT EXISTS source_text text,
    ADD COLUMN IF NOT EXISTS delivered_at timestamptz,
    ADD COLUMN IF NOT EXISTS repair_reason text;
