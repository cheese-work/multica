SET LOCAL lock_timeout = '2s';

ALTER TABLE comment
    DROP COLUMN IF EXISTS decision_comment_kind,
    DROP COLUMN IF EXISTS decision_intent_id,
    DROP COLUMN IF EXISTS decision_answer_id,
    DROP COLUMN IF EXISTS decision_request_id;
DROP TABLE IF EXISTS decision_intent;
DROP TABLE IF EXISTS decision_request;
