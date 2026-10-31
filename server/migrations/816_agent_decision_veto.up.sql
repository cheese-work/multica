-- Veto is a protocol event; the new event is checked for every new row.
ALTER TABLE governed_vote_record DROP CONSTRAINT governed_vote_record_decision_event_check;
ALTER TABLE governed_vote_record ADD CONSTRAINT governed_vote_record_decision_event_check
    CHECK (decision_event IN ('proposal', 'commit', 'vote', 'void', 'retract', 'veto')) NOT VALID;

-- A revocation names who undoes the work that relied on the record. Both columns
-- are set with revoked_at and never change after.
ALTER TABLE decision_agent_settlement ADD COLUMN undo_owner_type text, ADD COLUMN undo_owner_id uuid,
    ADD CONSTRAINT decision_agent_settlement_undo_owner_check CHECK (
        (revoked_at IS NULL AND undo_owner_type IS NULL AND undo_owner_id IS NULL)
        OR (revoked_at IS NOT NULL AND undo_owner_type IN ('agent', 'member') AND undo_owner_id IS NOT NULL));

-- A settlement stays insert-only except for one one-way revocation: revoked_at
-- may move from NULL to a value, with revoked_by and the undo owner set, and
-- nothing else changes. A revoked row is a permanent tombstone, so it can never
-- become effective again.
CREATE FUNCTION preserve_agent_decision_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF current_setting('multica.workspace_teardown', true) = 'on'
           OR NOT EXISTS (SELECT 1 FROM issue WHERE id = OLD.issue_id AND workspace_id = OLD.workspace_id) THEN
            RETURN OLD;
        END IF;
        RAISE EXCEPTION 'agent decision settlement is insert-only' USING ERRCODE = '23514';
    END IF;
    IF OLD.revoked_at IS NULL AND NEW.revoked_at IS NOT NULL AND NEW.revoked_by IS NOT NULL
       AND NEW.undo_owner_id IS NOT NULL
       AND NEW.instance_id = OLD.instance_id AND NEW.workspace_id = OLD.workspace_id
       AND NEW.issue_id = OLD.issue_id AND NEW.effective_at = OLD.effective_at THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'agent decision settlement allows only one-way revocation' USING ERRCODE = '23514';
END;
$$;
DROP TRIGGER agent_decision_settlement_immutable ON decision_agent_settlement;
CREATE TRIGGER agent_decision_settlement_immutable BEFORE UPDATE OR DELETE ON decision_agent_settlement
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_settlement();

-- One undo assignment per reliance that a revocation reached. The row names the
-- relying task and agent; it is insert-only like the reliance it answers.
CREATE TABLE decision_agent_undo (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reliance_id uuid NOT NULL UNIQUE,
    instance_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    task_id uuid NOT NULL,
    agent_id uuid NOT NULL,
    purpose text NOT NULL,
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER agent_decision_undo_immutable BEFORE UPDATE OR DELETE ON decision_agent_undo
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_scope();
