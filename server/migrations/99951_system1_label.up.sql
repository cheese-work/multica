SET LOCAL lock_timeout = '2s';

-- System 2 label of record for a System 1 observation (CHE-4383 B2b). Many rows
-- per observation are possible (later: labeller corrections), so it is its own
-- table. No foreign key: observation_id is checked in application code.
CREATE TABLE system1_label (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    observation_id uuid NOT NULL,
    scope text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('burn_in', 'eval')),
    source text NOT NULL CHECK (source IN ('human_answer')),
    -- waive | refuse for the test-first waiver scope; unmapped = free text or an unknown option key, excluded from the agreement denominator.
    label text NOT NULL,
    -- Set when the human answer is disclaimed; a superseded row is never the label of record.
    superseded_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Answer side: a decision whose stored inputs carry `topic` + `option_keys` (B2a) is labelled through the stored key order, never by position. A free-text answer or an unknown key is `unmapped`.
CREATE OR REPLACE FUNCTION system1_record_answer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    obs system1_observation%ROWTYPE;
    mapped text;
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
    WHERE kind = 'decision' AND subject_id = NEW.decision_request_id AND answered_at IS NULL
    RETURNING * INTO obs;

    -- A replacement answer after a withdraw/disclaim finds the observation already answered: label it too, once no live label remains.
    IF obs.id IS NULL THEN
        SELECT * INTO obs FROM system1_observation
        WHERE kind = 'decision' AND subject_id = NEW.decision_request_id ORDER BY created_at DESC LIMIT 1;
    END IF;

    IF obs.id IS NOT NULL AND obs.inputs ->> 'topic' = 'test_first_waiver'
       AND NOT EXISTS (SELECT 1 FROM system1_label WHERE observation_id = obs.id AND source = 'human_answer' AND superseded_at IS NULL) THEN
        mapped := obs.inputs -> 'option_keys' ->> NEW.chosen_option_index;
        INSERT INTO system1_label (workspace_id, observation_id, scope, kind, source, label)
        VALUES (obs.workspace_id, obs.id, obs.inputs ->> 'topic', 'burn_in', 'human_answer',
                CASE WHEN mapped IN ('waive', 'refuse') THEN mapped ELSE 'unmapped' END);
    END IF;
    RETURN NEW;
END $$;

-- Disclaim: the reversal also supersedes the label.
CREATE OR REPLACE FUNCTION system1_record_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE system1_observation SET reversed_at = NEW.disclaimed_at
    WHERE kind = 'decision' AND subject_id = NEW.decision_request_id AND reversed_at IS NULL;
    UPDATE system1_label SET superseded_at = NEW.disclaimed_at
    WHERE source = 'human_answer' AND superseded_at IS NULL
      AND observation_id IN (SELECT id FROM system1_observation WHERE kind = 'decision' AND subject_id = NEW.decision_request_id);
    RETURN NEW;
END $$;
