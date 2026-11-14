-- Barrier: refuse while any new server holds its shared lease (5266, 1). The
-- raise ends the transaction with no change.
DO $$
BEGIN
    IF NOT pg_try_advisory_xact_lock(5266, 1) THEN
        RAISE EXCEPTION 'new servers still running: stop them first';
    END IF;
END $$;

LOCK TABLE run_input IN SHARE ROW EXCLUSIVE MODE;

DROP TRIGGER IF EXISTS run_input_retire_held ON run_input;
DROP FUNCTION IF EXISTS run_input_retire_held();

CREATE OR REPLACE FUNCTION run_input_from_coalesced_comments() RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'queued' OR NEW.issue_id IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload)
    SELECT a.workspace_id, NEW.issue_id, NEW.agent_id, 'comment:' || c.id::text, 'comment',
           jsonb_build_object('comment_id', c.id::text, 'task_id', NEW.id::text,
                              'content', left(COALESCE(cm.content, ''), 2000))
    FROM agent a,
         unnest(NEW.coalesced_comment_ids || CASE WHEN NEW.trigger_comment_id IS NULL THEN '{}'::uuid[] ELSE ARRAY[NEW.trigger_comment_id] END) AS c(id)
         LEFT JOIN comment cm ON cm.id = c.id
    WHERE a.id = NEW.agent_id
    ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Every owned row goes to a member: parked, no owner.
UPDATE run_input SET state = 'parked', park_reason = 'legacy_unowned', owner_task_id = NULL
 WHERE state = 'owned';

ALTER TABLE run_input DROP CONSTRAINT IF EXISTS run_input_owned_has_owner;
ALTER TABLE run_input DROP CONSTRAINT IF EXISTS run_input_state_check;
ALTER TABLE run_input ADD CONSTRAINT run_input_state_check
    CHECK (state IN ('held', 'claimed', 'delivered', 'parked', 'discarded'));
ALTER TABLE run_input DROP CONSTRAINT IF EXISTS run_input_seq_key;
ALTER TABLE run_input DROP COLUMN IF EXISTS owner_task_id, DROP COLUMN IF EXISTS thread_key;
