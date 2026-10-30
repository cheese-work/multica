-- Issue/request transitions preserve episodes even when a wait ends and begins
-- between worker scans. Catalog edits are reconciled by the same worker.
CREATE FUNCTION sync_decision_missing_wait(target uuid) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM issue WHERE id = target FOR UPDATE;
    DELETE FROM decision_missing_wait d WHERE d.issue_id = target
      AND NOT EXISTS (SELECT 1 FROM decision_missing_candidate c
          WHERE c.issue_id = d.issue_id AND c.workspace_id = d.workspace_id);
    INSERT INTO decision_missing_wait (workspace_id, issue_id)
      SELECT workspace_id, issue_id FROM decision_missing_candidate WHERE issue_id = target
      ON CONFLICT (workspace_id, issue_id) DO NOTHING;
END;
$$;

CREATE FUNCTION track_decision_missing_wait() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'issue' THEN
        IF TG_OP = 'DELETE' THEN
            DELETE FROM decision_missing_wait WHERE issue_id = OLD.id;
        ELSE
            PERFORM sync_decision_missing_wait(NEW.id);
        END IF;
    ELSE
        IF TG_OP <> 'INSERT' THEN PERFORM sync_decision_missing_wait(OLD.issue_id); END IF;
        IF TG_OP <> 'DELETE' THEN PERFORM sync_decision_missing_wait(NEW.issue_id); END IF;
    END IF;
    RETURN NULL;
END;
$$;

CREATE TRIGGER decision_missing_issue AFTER INSERT OR UPDATE OF status, properties, workspace_id OR DELETE ON issue
FOR EACH ROW EXECUTE FUNCTION track_decision_missing_wait();
CREATE TRIGGER decision_missing_request AFTER INSERT OR UPDATE OF status, issue_id, workspace_id OR DELETE ON decision_request
FOR EACH ROW EXECUTE FUNCTION track_decision_missing_wait();

-- Existing waits receive a full grace period after installation.
INSERT INTO decision_missing_wait (workspace_id, issue_id)
SELECT workspace_id, issue_id FROM decision_missing_candidate
ON CONFLICT (workspace_id, issue_id) DO NOTHING;
