SET LOCAL lock_timeout = '2s';

-- The run-budget block marker (run_budget_notice, kind 'blocked') says the
-- budget put the issue in its current blocked status. Any write that takes the
-- issue out of blocked ends that block, so the marker goes with it: a later
-- block by another writer in the same window is not the budget's, and gets no
-- recovery run. A trigger covers every status writer.
CREATE OR REPLACE FUNCTION run_budget_block_marker_clear() RETURNS trigger AS $$
BEGIN
    DELETE FROM run_budget_notice WHERE issue_id = OLD.id AND kind = 'blocked';
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS issue_run_budget_block_marker_clear ON issue;
CREATE TRIGGER issue_run_budget_block_marker_clear AFTER UPDATE OF status ON issue
    FOR EACH ROW WHEN (OLD.status = 'blocked' AND NEW.status IS DISTINCT FROM OLD.status)
    EXECUTE FUNCTION run_budget_block_marker_clear();
