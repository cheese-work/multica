DROP TRIGGER IF EXISTS system1_answer_disclaimed ON decision_answer;
DROP TRIGGER IF EXISTS system1_answer_inserted ON decision_answer;
DROP FUNCTION IF EXISTS system1_record_reversal();
DROP FUNCTION IF EXISTS system1_record_answer();
DROP TABLE IF EXISTS system1_observation;
