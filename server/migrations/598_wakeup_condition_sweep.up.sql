-- Custom condition rules (CHE-1082 L10). The activation sweep brings the issues
-- that exist when a condition definition is written into the rule, a bounded
-- batch at a time. Its state lives on the definition itself: the revision the
-- sweep belongs to, a keyset cursor over the scope's issues, whether it is
-- done, whether it baselines facts already true (an edit) instead of acting on
-- them (a new rule), and the lease and retry bookkeeping of one pass. A pass
-- leases the definition by pushing sweep_retry_at forward, so concurrent
-- schedulers take different definitions and a crashed pass is retried when the
-- lease lapses. A revised definition is pending again by revision alone; its
-- old cursor is discarded when the next pass claims it, never reused. Existing
-- rows are pending until a pass settles them, which for anything that is not a
-- condition rule is one cheap update. A pending definition is found by the
-- partial index added by 599.
ALTER TABLE issue_wakeup_definition
 ADD COLUMN IF NOT EXISTS sweep_revision bigint NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS sweep_cursor uuid,
 ADD COLUMN IF NOT EXISTS sweep_done boolean NOT NULL DEFAULT false,
 ADD COLUMN IF NOT EXISTS sweep_baseline boolean NOT NULL DEFAULT false,
 ADD COLUMN IF NOT EXISTS sweep_attempts integer NOT NULL DEFAULT 0,
 ADD COLUMN IF NOT EXISTS sweep_retry_at timestamptz,
 ADD COLUMN IF NOT EXISTS sweep_error text;

-- 'issue.activate' is the signal an issue gives when it becomes newly eligible
-- for a condition rule. A condition definition lists it in event_types so the
-- capture trigger keeps it; it is no ordinary event, so "any event wanted" (the
-- early exit of the comment and issue-update triggers) ignores it.
CREATE OR REPLACE FUNCTION wakeup_scoped_event_probe(uuid, text, uuid, text) RETURNS boolean LANGUAGE plpgsql STABLE AS $$
BEGIN
 RETURN EXISTS (SELECT 1 FROM issue_wakeup_definition d WHERE d.workspace_id=$1 AND d.scope_kind=$2 AND d.scope_id=$3
  AND (CASE WHEN $4 IS NULL THEN cardinality(array_remove(d.event_types,'issue.activate'))>0 ELSE $4=ANY(d.event_types) END));
END $$;

-- A new issue, an issue moved into another project, one that changed status and
-- one that changed assignee (the dynamic target) may newly qualify for a
-- condition rule. The signal goes to the L9
-- outbox in the source transaction, only when a condition definition of the
-- issue's workspace, current project or own scope asked for it: the same
-- indexed probe as every other capture, so an issue write pays nothing more
-- where no condition rule exists.
CREATE OR REPLACE FUNCTION capture_issue_activation_wakeup() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='UPDATE' AND NEW.project_id IS NOT DISTINCT FROM OLD.project_id AND NEW.status IS NOT DISTINCT FROM OLD.status
  AND NEW.assignee_type IS NOT DISTINCT FROM OLD.assignee_type AND NEW.assignee_id IS NOT DISTINCT FROM OLD.assignee_id THEN RETURN NEW; END IF;
 IF wakeup_scoped_event_wanted(NEW.id,'issue.activate') THEN
  PERFORM capture_issue_wakeup(NEW.id,'issue.activate',gen_random_uuid()::text,NULL,NULL,'{}'::jsonb);
 END IF;
 RETURN NEW;
END $$;
DROP TRIGGER IF EXISTS capture_issue_activation_wakeup ON issue;
CREATE TRIGGER capture_issue_activation_wakeup AFTER INSERT OR UPDATE OF project_id, status, assignee_type, assignee_id ON issue FOR EACH ROW EXECUTE FUNCTION capture_issue_activation_wakeup();
