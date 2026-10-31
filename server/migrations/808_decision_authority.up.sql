ALTER TABLE decision_request
    ADD COLUMN authority text NOT NULL DEFAULT 'human'
        CHECK (authority IN ('human', 'agents'));
