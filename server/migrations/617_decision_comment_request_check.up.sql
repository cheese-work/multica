SET LOCAL lock_timeout = '2s';

ALTER TABLE comment
    ADD CONSTRAINT comment_decision_request_check CHECK (decision_comment_kind IS NULL OR decision_request_id IS NOT NULL) NOT VALID;
