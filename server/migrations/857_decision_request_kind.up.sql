ALTER TABLE decision_request
    ADD COLUMN kind text NOT NULL DEFAULT 'decision',
    ADD COLUMN expires_at timestamptz;
ALTER TABLE decision_request ADD CONSTRAINT decision_request_kind_check CHECK (kind IN ('decision', 'action'));
