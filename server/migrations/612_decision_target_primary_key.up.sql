DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'decision_intent_target'::regclass AND contype = 'p') THEN
        ALTER TABLE decision_intent_target ADD CONSTRAINT decision_intent_target_pkey PRIMARY KEY USING INDEX decision_intent_target_uidx;
    END IF;
END;
$$;
