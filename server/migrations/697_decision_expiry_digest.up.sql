SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_request DROP CONSTRAINT decision_request_cancel_reason_check;
ALTER TABLE decision_request ADD CONSTRAINT decision_request_cancel_reason_check
    CHECK (cancel_reason IN ('issue_closed', 'human_removed', 'agent_archived', 'lane_archived', 'record_disclaimed', 'record_vetoed', 'agent_withdrawn', 'expired'));

CREATE TABLE decision_expiry_digest (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    human_user_id uuid NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    digest_day date NOT NULL,
    inbox_item_id uuid REFERENCES inbox_item(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (workspace_id, human_user_id, digest_day)
);

CREATE TABLE decision_expiry_digest_request (
    request_id uuid PRIMARY KEY REFERENCES decision_request(id) ON DELETE CASCADE,
    digest_id uuid NOT NULL REFERENCES decision_expiry_digest(id) ON DELETE CASCADE
);

CREATE INDEX decision_expiry_digest_request_digest_idx ON decision_expiry_digest_request(digest_id);
CREATE INDEX decision_request_expiry_idx ON decision_request(created_at, id)
    WHERE status = 'open' AND origin = 'asked';
CREATE INDEX decision_request_expiry_digest_idx ON decision_request(workspace_id, human_user_id, id)
    WHERE status = 'cancelled' AND cancel_reason = 'expired';
