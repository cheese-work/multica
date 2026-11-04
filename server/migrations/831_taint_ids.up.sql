SET LOCAL lock_timeout = '2s';

-- Taint IDs (CHE-2216 plan 3.7). A task or comment carries the union of the
-- taint IDs of everything that fed it; IDs only grow. Propagation lives in
-- triggers because tasks are inserted from many paths and none may skip it.
-- IDs are opaque text, one per intake item. No foreign keys (repo rule).
ALTER TABLE agent_task_queue ADD COLUMN IF NOT EXISTS taint_ids text[] NOT NULL DEFAULT '{}';
ALTER TABLE comment ADD COLUMN IF NOT EXISTS taint_ids text[] NOT NULL DEFAULT '{}';

-- A comment written by a task stores that task's taint IDs.
CREATE OR REPLACE FUNCTION comment_inherit_taint() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.taint_ids := NEW.taint_ids || OLD.taint_ids;
    END IF;
    IF NEW.source_task_id IS NOT NULL THEN
        NEW.taint_ids := NEW.taint_ids || COALESCE((SELECT taint_ids FROM agent_task_queue WHERE id = NEW.source_task_id), '{}');
    END IF;
    NEW.taint_ids := COALESCE((SELECT array_agg(DISTINCT t ORDER BY t) FROM unnest(NEW.taint_ids) AS t), '{}');
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER comment_inherit_taint BEFORE INSERT OR UPDATE OF taint_ids, source_task_id ON comment
    FOR EACH ROW EXECUTE FUNCTION comment_inherit_taint();

-- A task inherits from the task it continues (parent, retry, rerun), the
-- comments that trigger it, and the task that delegated it. A resumed session
-- is chosen at claim and an assigning task is known only to the request, so
-- both write taint_ids directly (InheritSessionTaint, createAgentTask); so does
-- a wakeup, from its receipts (InheritWakeupReceiptTaint). The trigger unions.
CREATE OR REPLACE FUNCTION agent_task_inherit_taint() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.taint_ids := NEW.taint_ids || OLD.taint_ids;
    END IF;
    NEW.taint_ids := NEW.taint_ids
        || COALESCE((SELECT array_agg(t) FROM agent_task_queue q, unnest(q.taint_ids) AS t
            WHERE q.id IN (NEW.parent_task_id, NEW.retry_of_task_id, NEW.rerun_of_task_id, NEW.delegated_from_task_id, NEW.escalation_for_task_id)), '{}')
        || COALESCE((SELECT array_agg(t) FROM comment c, unnest(c.taint_ids) AS t
            WHERE c.id = NEW.trigger_comment_id OR c.id = ANY (COALESCE(NEW.coalesced_comment_ids, '{}'))), '{}');
    NEW.taint_ids := COALESCE((SELECT array_agg(DISTINCT t ORDER BY t) FROM unnest(NEW.taint_ids) AS t), '{}');
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER agent_task_inherit_taint BEFORE INSERT OR UPDATE OF taint_ids, parent_task_id, retry_of_task_id, rerun_of_task_id,
    delegated_from_task_id, escalation_for_task_id, trigger_comment_id, coalesced_comment_ids ON agent_task_queue
    FOR EACH ROW EXECUTE FUNCTION agent_task_inherit_taint();

-- A wakeup receipt coalesces: a later event replaces the payload, so the comment
-- that carried the taint can drop out of it. The receipt accumulates the taint
-- of every comment it has held, and the task the wakeup starts or merges into
-- takes it from there.
ALTER TABLE issue_wakeup_receipt ADD COLUMN IF NOT EXISTS taint_ids text[] NOT NULL DEFAULT '{}';

CREATE OR REPLACE FUNCTION wakeup_receipt_inherit_taint() RETURNS trigger AS $$
DECLARE
    comment_ref text := NEW.payload->>'comment_id';
BEGIN
    IF TG_OP = 'UPDATE' THEN
        NEW.taint_ids := NEW.taint_ids || OLD.taint_ids;
    END IF;
    IF comment_ref ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        NEW.taint_ids := NEW.taint_ids || COALESCE((SELECT taint_ids FROM comment WHERE id = comment_ref::uuid), '{}');
    END IF;
    NEW.taint_ids := COALESCE((SELECT array_agg(DISTINCT t ORDER BY t) FROM unnest(NEW.taint_ids) AS t), '{}');
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER wakeup_receipt_inherit_taint BEFORE INSERT OR UPDATE OF payload ON issue_wakeup_receipt
    FOR EACH ROW EXECUTE FUNCTION wakeup_receipt_inherit_taint();
