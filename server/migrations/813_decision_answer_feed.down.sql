DROP TRIGGER IF EXISTS decision_request_class_bumps_answers ON decision_request;
DROP FUNCTION IF EXISTS decision_request_class_bump_answers();
DROP TRIGGER IF EXISTS decision_answer_state_version ON decision_answer;
DROP FUNCTION IF EXISTS decision_answer_bump_state_version();
ALTER TABLE personal_access_token DROP COLUMN IF EXISTS scopes;
ALTER TABLE decision_answer
    DROP COLUMN IF EXISTS hindsight_acked_version,
    DROP COLUMN IF EXISTS state_version;
