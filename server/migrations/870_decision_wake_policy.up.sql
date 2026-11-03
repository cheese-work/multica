SET LOCAL lock_timeout = '2s';

-- A non-reversible request is answered on the card only. This refuses the wake
-- acknowledgement of an agent_record answer for any such request, so a server
-- binary that predates the delivery policy check cannot deliver one: the error
-- rolls back every side effect of the delivery transaction. The lifecycle skip
-- (wake_outcome = 'skipped_ineligible') delivers nothing and stays allowed.
-- Answers woken before this migration are not changed.
CREATE OR REPLACE FUNCTION decision_answer_refuse_forbidden_wake() RETURNS trigger AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM decision_request WHERE id = NEW.decision_request_id AND escalation_class <> 'reversible') THEN
        RAISE EXCEPTION 'decision answer % is an agent record for a non-reversible request; it needs a card answer', NEW.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS decision_answer_wake_policy ON decision_answer;
CREATE TRIGGER decision_answer_wake_policy BEFORE UPDATE OF woken_at ON decision_answer
    FOR EACH ROW WHEN (OLD.woken_at IS NULL AND NEW.woken_at IS NOT NULL
        AND NEW.wake_outcome IS DISTINCT FROM 'skipped_ineligible' AND NEW.answer_source = 'agent_record')
    EXECUTE FUNCTION decision_answer_refuse_forbidden_wake();
