SET LOCAL lock_timeout = '2s';

-- Issue-level taint (CHE-2216 plan 3.7). A task or comment that carries taint
-- IDs taints its issue until a member clears those IDs; while any ID is active
-- no run on the issue resolves elevated. No foreign keys (repo rule): the
-- handler validates workspace, issue and member.
-- ponytail: no secondary indexes; both tables stay small (one row per tainted
-- issue, one per member clear). Add a CONCURRENTLY index if a scan shows up.
CREATE TABLE IF NOT EXISTS issue_taint (
    issue_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    taint_ids text[] NOT NULL DEFAULT '{}',
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- The record of every member clear: who, when, which taint IDs. A cleared ID
-- never taints this issue again; a task that inherits it stays tainted itself.
CREATE TABLE IF NOT EXISTS issue_taint_clear (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL,
    issue_id uuid NOT NULL,
    taint_ids text[] NOT NULL CHECK (cardinality(taint_ids) >= 1),
    cleared_by_member_id uuid NOT NULL,
    cleared_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- Adds p_ids (minus IDs a member already cleared for the issue) to the issue's
-- active taint. The issue row lock serializes with an authorization in flight
-- (it takes a share lock on the same row) and with the member clear.
CREATE OR REPLACE FUNCTION issue_taint_note(p_issue uuid, p_ids text[]) RETURNS void AS $$
DECLARE
    ws uuid;
    fresh text[];
BEGIN
    IF p_issue IS NULL OR cardinality(p_ids) = 0 THEN
        RETURN;
    END IF;
    SELECT workspace_id INTO ws FROM issue WHERE id = p_issue FOR NO KEY UPDATE;
    IF ws IS NULL THEN
        RETURN;
    END IF;
    SELECT COALESCE(array_agg(DISTINCT t ORDER BY t), '{}') INTO fresh FROM unnest(p_ids) AS t
        WHERE NOT EXISTS (SELECT 1 FROM issue_taint_clear c WHERE c.issue_id = p_issue AND t = ANY (c.taint_ids));
    IF cardinality(fresh) = 0 THEN
        RETURN;
    END IF;
    INSERT INTO issue_taint (issue_id, workspace_id, taint_ids) VALUES (p_issue, ws, fresh)
    ON CONFLICT (issue_id) DO UPDATE SET
        taint_ids = (SELECT array_agg(DISTINCT t ORDER BY t) FROM unnest(issue_taint.taint_ids || EXCLUDED.taint_ids) AS t),
        updated_at = clock_timestamp();
END
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION issue_taint_from_row() RETURNS trigger AS $$
BEGIN
    PERFORM issue_taint_note(NEW.issue_id, NEW.taint_ids);
    RETURN NULL;
END
$$ LANGUAGE plpgsql;

-- Runs after the B5a inheritance triggers, so NEW.taint_ids is final. Updates
-- fire only when the IDs changed, so a status write on a tainted task takes no lock.
CREATE TRIGGER agent_task_issue_taint_ins AFTER INSERT ON agent_task_queue
    FOR EACH ROW WHEN (cardinality(NEW.taint_ids) > 0) EXECUTE FUNCTION issue_taint_from_row();
CREATE TRIGGER agent_task_issue_taint_upd AFTER UPDATE ON agent_task_queue
    FOR EACH ROW WHEN (NEW.taint_ids IS DISTINCT FROM OLD.taint_ids AND cardinality(NEW.taint_ids) > 0) EXECUTE FUNCTION issue_taint_from_row();
CREATE TRIGGER comment_issue_taint_ins AFTER INSERT ON comment
    FOR EACH ROW WHEN (cardinality(NEW.taint_ids) > 0) EXECUTE FUNCTION issue_taint_from_row();
CREATE TRIGGER comment_issue_taint_upd AFTER UPDATE ON comment
    FOR EACH ROW WHEN (NEW.taint_ids IS DISTINCT FROM OLD.taint_ids AND cardinality(NEW.taint_ids) > 0) EXECUTE FUNCTION issue_taint_from_row();

-- Taint written before this migration.
INSERT INTO issue_taint (issue_id, workspace_id, taint_ids)
SELECT s.issue_id, i.workspace_id, array_agg(DISTINCT s.t ORDER BY s.t)
FROM (
    SELECT q.issue_id, t FROM agent_task_queue q, unnest(q.taint_ids) AS t WHERE q.issue_id IS NOT NULL
    UNION ALL
    SELECT c.issue_id, t FROM comment c, unnest(c.taint_ids) AS t
) s JOIN issue i ON i.id = s.issue_id
GROUP BY s.issue_id, i.workspace_id
ON CONFLICT (issue_id) DO NOTHING;
