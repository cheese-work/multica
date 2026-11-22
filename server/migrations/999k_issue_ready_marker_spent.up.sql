-- The author's ready marker (CHE-5741) covers one handoff. Spending it in the
-- database catches every status write: a CI advance, a person's or agent's
-- manual move, a decision or merge move. A marker set after the move survives.
CREATE OR REPLACE FUNCTION spend_issue_ready_marker() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    NEW.metadata := NEW.metadata - 'ready_for_review';
    RETURN NEW;
END $$;

CREATE TRIGGER issue_ready_marker_spent BEFORE UPDATE OF status ON issue
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status AND OLD.metadata ? 'ready_for_review')
    EXECUTE FUNCTION spend_issue_ready_marker();
