DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM decision_agent_undo)
       OR EXISTS (SELECT 1 FROM decision_agent_settlement WHERE revoked_at IS NOT NULL)
       OR EXISTS (SELECT 1 FROM governed_vote_record WHERE decision_event = 'veto') THEN
        RAISE EXCEPTION 'cannot downgrade with agent decision vetoes or revocations';
    END IF;
END; $$;
DROP TABLE decision_agent_undo;
DROP TRIGGER agent_decision_settlement_immutable ON decision_agent_settlement;
DROP FUNCTION preserve_agent_decision_settlement();
CREATE TRIGGER agent_decision_settlement_immutable BEFORE UPDATE OR DELETE ON decision_agent_settlement
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_scope();
ALTER TABLE governed_vote_record DROP CONSTRAINT governed_vote_record_decision_event_check;
ALTER TABLE governed_vote_record ADD CONSTRAINT governed_vote_record_decision_event_check
    CHECK (decision_event IN ('proposal', 'commit', 'vote', 'void', 'retract')) NOT VALID;
