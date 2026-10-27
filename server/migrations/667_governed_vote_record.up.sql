-- CHE-1862: append-only record of the vote and proposal lines of every
-- agent/member comment, taken when the comment is created. The vote gate reads
-- this table, never the comment, so an owner/admin who edits or deletes another
-- author's comment cannot forge a PASS or erase a BLOCK. The author's runtime
-- provider and model are kept as they were when the vote was cast.
CREATE TABLE governed_vote_record (
    issue_id UUID NOT NULL,
    comment_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    author_type TEXT NOT NULL,
    author_id UUID NOT NULL,
    vote_lines TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    runtime_provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (issue_id, comment_id)
);

CREATE FUNCTION record_governed_vote() RETURNS trigger
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

CREATE TRIGGER comment_record_governed_vote
AFTER INSERT ON comment
FOR EACH ROW
WHEN (NEW.author_type IN ('agent', 'member')
      AND (NEW.content LIKE '%Vote:%' OR NEW.content LIKE '%Proposal:%'))
EXECUTE FUNCTION record_governed_vote();
