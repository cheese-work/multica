-- Epic-done completion episodes (CHE-3550, CHE-3473 M13). One row per (lane,
-- root) once its epic-done predicate was first evaluated. revision names the
-- current completion episode; fired_at is set when that episode fired. A reopen
-- of the root or of a tracked descendant, or a new unfinished tracked child,
-- ends a fired episode in the writer's own transaction: revision + 1, fired_at
-- cleared. No FKs: issue, chat session and workspace delete sweep this table in
-- application code.
CREATE TABLE dm_progress_epic_episode (
    lineage_id UUID NOT NULL,
    root_issue_id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    fired_at TIMESTAMPTZ,
    PRIMARY KEY (lineage_id, root_issue_id)
);

-- A status is unfinished unless its lifecycle category is done or closed. A
-- custom status resolves through the catalog; the two built-in terminal keys
-- fall back to their fixed categories when the catalog has no row for them.
-- An unknown status counts as unfinished.
CREATE FUNCTION dm_progress_epic_unfinished(p_workspace uuid, p_status text) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT COALESCE(
  (SELECT s.category NOT IN ('done', 'closed') FROM issue_status s WHERE s.workspace_id = p_workspace AND s.key = p_status),
  p_status NOT IN ('done', 'cancelled'))
$$;

CREATE FUNCTION dm_progress_epic_reopen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF dm_progress_epic_unfinished(NEW.workspace_id, NEW.status) THEN
  UPDATE dm_progress_epic_episode e SET revision = e.revision + 1, fired_at = NULL
  FROM dm_progress_follow f
  WHERE f.issue_id = NEW.id AND f.state = 'active' AND e.lineage_id = f.lineage_id
    AND e.root_issue_id = f.reporting_root_id AND e.fired_at IS NOT NULL;
 END IF;
 RETURN NULL;
END $$;

CREATE FUNCTION dm_progress_epic_new_child() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF dm_progress_epic_unfinished(NEW.workspace_id, (SELECT i.status FROM issue i WHERE i.id = NEW.issue_id)) THEN
  UPDATE dm_progress_epic_episode e SET revision = e.revision + 1, fired_at = NULL
  WHERE e.lineage_id = NEW.lineage_id AND e.root_issue_id = NEW.reporting_root_id AND e.fired_at IS NOT NULL;
 END IF;
 RETURN NULL;
END $$;

CREATE TRIGGER dm_progress_epic_reopen AFTER UPDATE OF status ON issue
 FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION dm_progress_epic_reopen();

CREATE TRIGGER dm_progress_epic_new_child AFTER INSERT OR UPDATE OF state ON dm_progress_follow
 FOR EACH ROW WHEN (NEW.state = 'active') EXECUTE FUNCTION dm_progress_epic_new_child();
