-- Refuse destructive rollback while a live instance exists.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM decision_request WHERE authority = 'agents') THEN
        RAISE EXCEPTION 'cannot downgrade with agent decision instances';
    END IF;
END; $$;
DROP VIEW agent_decision_live_vote;
DROP TRIGGER agent_decision_record_immutable ON governed_vote_record;
DROP FUNCTION preserve_agent_decision_record();
DROP TRIGGER agent_decision_request_immutable ON decision_request;
DROP FUNCTION preserve_agent_decision_request();
DROP TRIGGER agent_decision_scope_immutable ON decision_agent_scope;
DROP FUNCTION preserve_agent_decision_scope();
DROP TABLE decision_agent_scope;
DROP TRIGGER comment_record_governed_vote ON comment;
CREATE TRIGGER comment_record_governed_vote AFTER INSERT ON comment FOR EACH ROW
WHEN (NEW.author_type IN ('agent', 'member') AND (NEW.content LIKE '%Vote:%' OR NEW.content LIKE '%Proposal:%'))
EXECUTE FUNCTION record_governed_vote();
CREATE OR REPLACE FUNCTION record_governed_vote() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lines TEXT;
BEGIN
    SELECT string_agg(btrim(line), E'\n') INTO lines
      FROM unnest(regexp_split_to_array(NEW.content, E'\r?\n')) AS line
     WHERE btrim(line) LIKE 'Vote:%' OR btrim(line) LIKE 'Proposal:%';
    IF lines IS NULL THEN
        RETURN NEW;
    END IF;
    IF lines LIKE '%Vote: BLOCK%' THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('governed_vote:' || NEW.issue_id::text, 0));
    END IF;
    INSERT INTO governed_vote_record
        (issue_id, comment_id, workspace_id, author_type, author_id, vote_lines, created_at, runtime_provider, model)
    SELECT NEW.issue_id, NEW.id, NEW.workspace_id, NEW.author_type, NEW.author_id, lines,
           NEW.created_at, COALESCE(r.provider, ''), COALESCE(t.claimed_model, '')
      FROM (SELECT 1) AS one
      LEFT JOIN agent_task_queue t
        ON NEW.author_type = 'agent' AND t.id = NEW.source_task_id AND t.agent_id = NEW.author_id
      LEFT JOIN agent_runtime r ON r.id = t.runtime_id;
    RETURN NEW;
END;
$$;

ALTER TABLE governed_vote_record DROP COLUMN full_body, DROP COLUMN source_task_id,
    DROP COLUMN decision_instance_id, DROP COLUMN decision_event, DROP COLUMN decision_verdict,
    DROP COLUMN commitment_sha256, DROP COLUMN decision_sequence;
ALTER TABLE decision_request DROP CONSTRAINT decision_human_identity,
    ALTER COLUMN human_user_id SET NOT NULL, ALTER COLUMN chat_session_id SET NOT NULL;
