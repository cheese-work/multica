CREATE OR REPLACE FUNCTION record_governed_vote() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO governed_vote_record
        (issue_id, comment_id, workspace_id, author_type, author_id, vote_lines, created_at, runtime_provider, model)
    SELECT NEW.issue_id, NEW.id, NEW.workspace_id, NEW.author_type, NEW.author_id,
           (SELECT string_agg(btrim(line), E'\n')
              FROM unnest(regexp_split_to_array(NEW.content, E'\r?\n')) AS line
             WHERE btrim(line) LIKE 'Vote:%' OR btrim(line) LIKE 'Proposal:%'),
           NEW.created_at, COALESCE(r.provider, ''), COALESCE(a.model, '')
      FROM (SELECT 1) AS one
      LEFT JOIN agent a ON NEW.author_type = 'agent' AND a.id = NEW.author_id
      LEFT JOIN agent_runtime r ON r.id = a.runtime_id
     WHERE EXISTS (SELECT 1 FROM unnest(regexp_split_to_array(NEW.content, E'\r?\n')) AS line
                    WHERE btrim(line) LIKE 'Vote:%' OR btrim(line) LIKE 'Proposal:%');
    RETURN NEW;
END;
$$;
ALTER TABLE agent_task_queue DROP COLUMN claimed_model;
