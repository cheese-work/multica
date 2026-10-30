-- CHE-1862 correction: bind a vote's lab identity to the task that cast it,
-- not to the agent row, which an agent's own task token can edit mid-run.
--
-- claimed_model is the model the daemon was told to run when it claimed the
-- task. The vote record then takes provider and model from the comment's
-- source task. A BLOCK also takes a per-issue advisory lock until its
-- transaction ends, so a governed write that holds the same lock sees every
-- BLOCK committed before it and delays any later one until the write commits.
ALTER TABLE agent_task_queue ADD COLUMN claimed_model TEXT;

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
