-- In-transaction capture for DM progress cards (CHE-3369 I1.2). Triggers cover
-- every writer of issue status, assignee and PR state, so a crash before
-- commit loses the mutation and its event together, and no writer (HTTP,
-- batch, PR automation, a future one) can skip capture.
-- A follow is "root" unless every reason is inherited; only a root entering
-- blocked (not already rendered blocked), done or cancelled is immediate (I2.2).
-- One event_id per change, shared by every lane that follows the issue (I1.1);
-- two transitions of the same class get two ids. Status and assignee keep no
-- history row, so the id is minted here, in the writer's transaction.
CREATE FUNCTION dm_progress_capture(p_issue uuid, p_kind text, p_to_status text) RETURNS void LANGUAGE sql AS $$
 WITH ev AS MATERIALIZED (SELECT gen_random_uuid() AS id)
 INSERT INTO dm_progress_pending(workspace_id, lineage_id, issue_id, event_kind, event_id, immediate)
 SELECT f.workspace_id, f.lineage_id, f.issue_id, p_kind, ev.id,
        p_kind = 'status'
        AND EXISTS (SELECT 1 FROM unnest(f.reasons) r WHERE r NOT LIKE 'inherited:%')
        AND ((p_to_status = 'blocked' AND f.last_rendered_status <> 'blocked') OR p_to_status IN ('done', 'cancelled'))
 FROM dm_progress_follow f, ev
 WHERE f.issue_id = p_issue AND f.state = 'active' AND f.muted_at IS NULL
$$;

CREATE FUNCTION dm_progress_capture_issue() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF OLD.status IS DISTINCT FROM NEW.status THEN
  PERFORM dm_progress_capture(NEW.id, 'status', NEW.status);
 END IF;
 IF OLD.assignee_type IS DISTINCT FROM NEW.assignee_type OR OLD.assignee_id IS DISTINCT FROM NEW.assignee_id THEN
  PERFORM dm_progress_capture(NEW.id, 'assignee', NULL);
 END IF;
 RETURN NULL;
END $$;

-- A PR linked to the issue (either provider).
CREATE FUNCTION dm_progress_capture_pr_link() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 PERFORM dm_progress_capture(NEW.issue_id, 'pr', NULL);
 RETURN NULL;
END $$;

-- A linked PR changed state (ready, merged, closed, reopened). TG_ARGV[0] is
-- the link table of the PR's provider.
CREATE FUNCTION dm_progress_capture_pr_state() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE linked uuid;
BEGIN
 FOR linked IN EXECUTE format('SELECT issue_id FROM %I WHERE pull_request_id = $1', TG_ARGV[0]) USING NEW.id LOOP
  PERFORM dm_progress_capture(linked, 'pr', NULL);
 END LOOP;
 RETURN NULL;
END $$;

CREATE TRIGGER dm_progress_capture_issue AFTER UPDATE OF status, assignee_type, assignee_id ON issue
 FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status OR OLD.assignee_type IS DISTINCT FROM NEW.assignee_type OR OLD.assignee_id IS DISTINCT FROM NEW.assignee_id)
 EXECUTE FUNCTION dm_progress_capture_issue();

CREATE TRIGGER dm_progress_capture_pr_link AFTER INSERT ON issue_pull_request
 FOR EACH ROW WHEN (NEW.link_kind = 'closing') EXECUTE FUNCTION dm_progress_capture_pr_link();
CREATE TRIGGER dm_progress_capture_pr_relink AFTER UPDATE OF link_kind ON issue_pull_request
 FOR EACH ROW WHEN (OLD.link_kind IS DISTINCT FROM NEW.link_kind AND NEW.link_kind = 'closing') EXECUTE FUNCTION dm_progress_capture_pr_link();
CREATE TRIGGER dm_progress_capture_pr_link AFTER INSERT ON issue_vcs_pull_request
 FOR EACH ROW EXECUTE FUNCTION dm_progress_capture_pr_link();

CREATE TRIGGER dm_progress_capture_pr_state AFTER UPDATE OF state ON github_pull_request
 FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state) EXECUTE FUNCTION dm_progress_capture_pr_state('issue_pull_request');
CREATE TRIGGER dm_progress_capture_pr_state AFTER UPDATE OF state ON vcs_pull_request
 FOR EACH ROW WHEN (OLD.state IS DISTINCT FROM NEW.state) EXECUTE FUNCTION dm_progress_capture_pr_state('issue_vcs_pull_request');
