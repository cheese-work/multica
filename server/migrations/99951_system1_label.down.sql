CREATE OR REPLACE FUNCTION system1_record_answer() RETURNS trigger LANGUAGE plpgsql AS $$
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

CREATE OR REPLACE FUNCTION system1_record_reversal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE system1_observation SET reversed_at = NEW.disclaimed_at
    WHERE kind = 'decision' AND subject_id = NEW.decision_request_id AND reversed_at IS NULL;
    RETURN NEW;
END $$;

DROP TABLE IF EXISTS system1_label;
