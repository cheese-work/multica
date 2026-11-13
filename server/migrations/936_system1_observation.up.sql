SET LOCAL lock_timeout = '2s';

-- Shadow System 1 capture (CHE-3628). One row per human decision request and
-- one per workflow routing decision. It holds features, option labels in the
-- order shown, and choices; never ask text, answer text, DM content or secrets.
-- Nothing reads a prediction to act on it.
CREATE TABLE system1_observation (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('decision', 'routing')),
    -- decision_request.id for a decision, issue.id for a routing decision.
    subject_id uuid NOT NULL,
    class text NOT NULL,
    -- Who made the choice. NULL while a decision request is still open.
    decision_source text CHECK (decision_source IN ('rule', 'agent', 'human')),
    inputs jsonb NOT NULL DEFAULT '{}',
    -- Number of options offered. Labels are ask text and are never stored.
    option_count integer NOT NULL DEFAULT 0 CHECK (option_count >= 0),
    recommended_index integer CHECK (recommended_index >= 0),
    agent_confidence real CHECK (agent_confidence BETWEEN 0 AND 1),
    -- Shadow prediction, written in the same transaction that creates the
    -- request, so it always precedes the human answer. Empty key = abstained.
    predicted_key text,
    predicted_confidence real,
    predicted_precedents integer,
    predicted_at timestamptz,
    -- recommended | other | option (no recommendation identified) | free_text for a decision; <handler_type>:<handler_id> for routing.
    choice_key text,
    chosen_index integer CHECK (chosen_index >= 0),
    answered_at timestamptz,
    time_to_answer_ms bigint,
    outcome text CHECK (outcome IN ('answered', 'applied')),
    -- A disclaimed answer, or a routing handler a person later replaced.
    reversed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);


-- Every answer path (card click, agent record) inserts a decision_answer, so one trigger sees all of them.
CREATE FUNCTION system1_record_answer() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE system1_observation SET
        decision_source = 'human',
        outcome = 'answered',
        chosen_index = NEW.chosen_option_index,
        choice_key = CASE
            WHEN NEW.chosen_option_index IS NULL THEN 'free_text'
            WHEN recommended_index IS NULL THEN 'option'
            WHEN NEW.chosen_option_index = recommended_index THEN 'recommended'
            ELSE 'other' END,
        answered_at = NEW.created_at,
        time_to_answer_ms = (extract(epoch FROM NEW.created_at - created_at) * 1000)::bigint
    WHERE kind = 'decision' AND subject_id = NEW.decision_request_id AND answered_at IS NULL;
    RETURN NEW;
END $$;

CREATE TRIGGER system1_answer_inserted AFTER INSERT ON decision_answer
    FOR EACH ROW EXECUTE FUNCTION system1_record_answer();

CREATE FUNCTION system1_record_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE system1_observation SET reversed_at = NEW.disclaimed_at
    WHERE kind = 'decision' AND subject_id = NEW.decision_request_id AND reversed_at IS NULL;
    RETURN NEW;
END $$;

CREATE TRIGGER system1_answer_disclaimed AFTER UPDATE OF disclaimed_at ON decision_answer
    FOR EACH ROW WHEN (OLD.disclaimed_at IS NULL AND NEW.disclaimed_at IS NOT NULL)
    EXECUTE FUNCTION system1_record_reversal();
