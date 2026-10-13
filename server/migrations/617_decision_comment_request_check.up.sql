SET LOCAL lock_timeout = '2s';

ALTER TABLE comment
    ADD CONSTRAINT comment_decision_request_check CHECK (
        decision_comment_kind IS NULL
        OR (decision_comment_kind IN ('ask', 'cancel')
            AND decision_request_id IS NOT NULL
            AND decision_answer_id IS NULL)
        OR (decision_comment_kind IN ('quote', 'disclaim', 'delivery', 'veto')
            AND decision_answer_id IS NOT NULL)
    ) NOT VALID;
