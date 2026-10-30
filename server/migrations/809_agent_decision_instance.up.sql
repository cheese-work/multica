-- Agent proposals have no human or DM, and never manufacture a human answer.
ALTER TABLE decision_request ALTER COLUMN human_user_id DROP NOT NULL,
    ALTER COLUMN chat_session_id DROP NOT NULL,
    ADD CONSTRAINT decision_human_identity CHECK
        (authority <> 'human' OR (human_user_id IS NOT NULL AND chat_session_id IS NOT NULL));

CREATE TABLE decision_agent_scope (
    instance_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    proposer_agent_id uuid NOT NULL,
    proposer_task_id uuid NOT NULL,
    proposer_provider text NOT NULL,
    proposer_model text NOT NULL,
    proposer_lab text NOT NULL CHECK (proposer_lab <> ''),
    work_author_id uuid NOT NULL CHECK (work_author_id = proposer_agent_id),
    candidate_sha256 text NOT NULL CHECK (candidate_sha256 ~ '^[0-9a-f]{64}$'),
    waived_gate text NOT NULL,
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'array'),
    route text NOT NULL CHECK (route IN ('cosign', 'adjudicate')),
    scope jsonb NOT NULL,
    delegation_id uuid NOT NULL,
    allowlist_version integer NOT NULL,
    proposal_body text NOT NULL,
    proposal_sha256 text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

ALTER TABLE governed_vote_record ADD COLUMN full_body text NOT NULL DEFAULT '',
    ADD COLUMN source_task_id uuid,
    ADD COLUMN decision_instance_id uuid,
    ADD COLUMN decision_event text CHECK (decision_event IN ('proposal', 'commit', 'vote', 'void', 'retract')),
    ADD COLUMN decision_verdict text CHECK (decision_verdict IN ('PASS', 'BLOCK')),
    ADD COLUMN commitment_sha256 text,
    ADD COLUMN decision_sequence bigint GENERATED ALWAYS AS IDENTITY;

-- Snapshot and lock ALL decision lines, including PASS and commitments.
-- Old governed-field BLOCK serialization and BLOCK-as-retraction stay intact.
CREATE OR REPLACE FUNCTION record_governed_vote() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lines text;
    instance text;
BEGIN
    SELECT string_agg(btrim(line), E'\n') INTO lines
      FROM unnest(regexp_split_to_array(NEW.content, E'\r?\n')) AS line
     WHERE btrim(line) LIKE 'Vote:%' OR btrim(line) LIKE 'Proposal:%'
        OR btrim(line) LIKE 'Commit:%' OR btrim(line) LIKE 'Retract:%' OR btrim(line) LIKE 'Veto:%';
    IF lines IS NULL THEN RETURN NEW; END IF;
    -- Ordered locks also make a malformed multi-instance insert safe before refusal.
    FOR instance IN SELECT DISTINCT match[1]
        FROM regexp_matches(lines, 'decision:([0-9a-fA-F-]{36})', 'g') AS match ORDER BY match[1]
    LOOP
        PERFORM pg_advisory_xact_lock(hashtextextended('agent-decision:' || instance::uuid::text, 0));
    END LOOP;
    IF lines LIKE '%Vote: BLOCK%' THEN
        PERFORM pg_advisory_xact_lock(hashtextextended('governed_vote:' || NEW.issue_id::text, 0));
    END IF;
    INSERT INTO governed_vote_record
        (issue_id, comment_id, workspace_id, author_type, author_id, vote_lines, created_at,
         runtime_provider, model, full_body, source_task_id)
    SELECT NEW.issue_id, NEW.id, NEW.workspace_id, NEW.author_type, NEW.author_id, lines,
           NEW.created_at, COALESCE(r.provider, ''), COALESCE(t.claimed_model, ''), NEW.content, NEW.source_task_id
      FROM (SELECT 1) AS one
      LEFT JOIN agent_task_queue t
        ON NEW.author_type = 'agent' AND t.id = NEW.source_task_id AND t.agent_id = NEW.author_id
      LEFT JOIN agent_runtime r ON r.id = t.runtime_id;
    RETURN NEW;
END;
$$;
DROP TRIGGER comment_record_governed_vote ON comment;
CREATE TRIGGER comment_record_governed_vote AFTER INSERT ON comment FOR EACH ROW
WHEN (NEW.author_type IN ('agent', 'member')) EXECUTE FUNCTION record_governed_vote();

CREATE FUNCTION preserve_agent_decision_scope() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('multica.workspace_teardown', true) = 'on' THEN RETURN OLD; END IF;
    IF TG_OP = 'DELETE' AND NOT EXISTS (SELECT 1 FROM issue WHERE id = OLD.issue_id AND workspace_id = OLD.workspace_id) THEN
        RETURN OLD; -- explicit orphan cleanup only
    END IF;
    RAISE EXCEPTION 'agent decision scope is insert-only' USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER agent_decision_scope_immutable BEFORE UPDATE OR DELETE ON decision_agent_scope
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_scope();

CREATE FUNCTION preserve_agent_decision_request() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('multica.workspace_teardown', true) = 'on' THEN RETURN OLD; END IF;
    IF EXISTS (SELECT 1 FROM decision_agent_scope WHERE instance_id = OLD.id) THEN
        RAISE EXCEPTION 'agent decision request is immutable' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER agent_decision_request_immutable BEFORE UPDATE OR DELETE ON decision_request
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_request();

CREATE FUNCTION preserve_agent_decision_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' AND current_setting('multica.workspace_teardown', true) = 'on' THEN RETURN OLD; END IF;
    IF OLD.vote_lines LIKE '%decision:%' THEN
        IF TG_OP = 'DELETE' AND NOT EXISTS (SELECT 1 FROM issue WHERE id = OLD.issue_id AND workspace_id = OLD.workspace_id) THEN RETURN OLD; END IF;
        -- The insert transaction may annotate an immutable snapshot exactly once.
        IF TG_OP = 'UPDATE' AND OLD.decision_instance_id IS NULL AND NEW.decision_instance_id IS NOT NULL
           AND (to_jsonb(OLD) - ARRAY['decision_instance_id','decision_event','decision_verdict','commitment_sha256'])
             = (to_jsonb(NEW) - ARRAY['decision_instance_id','decision_event','decision_verdict','commitment_sha256']) THEN RETURN NEW; END IF;
        RAISE EXCEPTION 'decision vote snapshot is immutable' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER agent_decision_record_immutable BEFORE UPDATE OR DELETE ON governed_vote_record
FOR EACH ROW EXECUTE FUNCTION preserve_agent_decision_record();

-- C1 has no settlement. This is a tally input only, never authorization.
CREATE VIEW agent_decision_live_vote AS
SELECT vote.decision_instance_id AS instance_id, vote.author_id, vote.decision_verdict AS verdict,
       vote.comment_id, vote.runtime_provider, vote.model, vote.full_body, vote.source_task_id
FROM governed_vote_record vote
WHERE vote.decision_event = 'vote'
  AND NOT EXISTS (SELECT 1 FROM governed_vote_record stopped
    WHERE stopped.decision_instance_id = vote.decision_instance_id AND stopped.author_id = vote.author_id
      AND stopped.decision_event IN ('void', 'retract'));
