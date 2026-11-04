SET LOCAL lock_timeout = '2s';

-- state_version is the change version of the Hindsight document built from an
-- answer; hindsight_acked_version is the highest version the X99 sweep has
-- written. The feed returns answers with state_version > hindsight_acked_version.
ALTER TABLE decision_answer
    ADD COLUMN IF NOT EXISTS state_version bigint NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS hindsight_acked_version bigint NOT NULL DEFAULT 0;

-- scopes narrows what a PAT may do beyond the owner's own access. Empty keeps
-- every existing token exactly as it was; 'decisions:feed' opens the answer feed.
ALTER TABLE personal_access_token
    ADD COLUMN IF NOT EXISTS scopes text[] NOT NULL DEFAULT '{}';

-- The version rises inside whatever transaction changes the document's input, so
-- no write path has to remember to do it: finalization (wake_at), wake ack
-- (woken_at, wake_outcome), disclaim, veto and retraction, and a hold that moves
-- effective_at. A class change of the request bumps its answers below.
CREATE OR REPLACE FUNCTION decision_answer_bump_state_version() RETURNS trigger AS $$
BEGIN
    IF (NEW.effective_at, NEW.wake_at, NEW.woken_at, NEW.wake_outcome, NEW.disclaimed_at, NEW.disclaim_reason, NEW.veto_reason, NEW.retraction_outcome)
       IS DISTINCT FROM
       (OLD.effective_at, OLD.wake_at, OLD.woken_at, OLD.wake_outcome, OLD.disclaimed_at, OLD.disclaim_reason, OLD.veto_reason, OLD.retraction_outcome) THEN
        NEW.state_version := OLD.state_version + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS decision_answer_state_version ON decision_answer;
CREATE TRIGGER decision_answer_state_version BEFORE UPDATE ON decision_answer
    FOR EACH ROW EXECUTE FUNCTION decision_answer_bump_state_version();

CREATE OR REPLACE FUNCTION decision_request_class_bump_answers() RETURNS trigger AS $$
BEGIN
    UPDATE decision_answer SET state_version = state_version + 1 WHERE decision_request_id = NEW.id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS decision_request_class_bumps_answers ON decision_request;
CREATE TRIGGER decision_request_class_bumps_answers AFTER UPDATE OF escalation_class ON decision_request
    FOR EACH ROW WHEN (OLD.escalation_class IS DISTINCT FROM NEW.escalation_class)
    EXECUTE FUNCTION decision_request_class_bump_answers();
