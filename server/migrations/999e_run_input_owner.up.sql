-- CHE-5266 slice 2: one ownership key through every run_input path.
-- One transaction. The table lock serializes old claims and completions against
-- the backfill and the triggers; the table is small and the lock lasts
-- milliseconds. Order: columns, CHECK swap, backfill, claimed-batch cutover,
-- then the triggers last.
LOCK TABLE run_input IN SHARE ROW EXCLUSIVE MODE;

-- Statement 1: columns and the state CHECK (superset keeps old writers valid).
ALTER TABLE run_input
    ADD COLUMN IF NOT EXISTS owner_task_id uuid,
    ADD COLUMN IF NOT EXISTS thread_key uuid;
-- seq is the row id the re-hold and recovery queries address by.
ALTER TABLE run_input ADD CONSTRAINT run_input_seq_key UNIQUE (seq);
ALTER TABLE run_input DROP CONSTRAINT IF EXISTS run_input_state_check;
ALTER TABLE run_input ADD CONSTRAINT run_input_state_check
    CHECK (state IN ('held', 'owned', 'claimed', 'delivered', 'parked', 'discarded')) NOT VALID;
ALTER TABLE run_input VALIDATE CONSTRAINT run_input_state_check;

-- Statement 2: thread evidence for every row, owner evidence for held rows.
-- T1: payload task id names a carrier of the same issue and agent.
-- T2: the input is a comment of the same issue; its thread is the root's.
-- O1: T1 matched and the carrier is active. There is no O2.
WITH base AS (
    SELECT r.seq, r.issue_id, r.agent_id,
           CASE WHEN r.payload->>'task_id' ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                THEN (r.payload->>'task_id')::uuid END AS ptask,
           CASE WHEN COALESCE(r.payload->>'comment_id', r.payload->>'trigger_comment_id')
                     ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                THEN COALESCE(r.payload->>'comment_id', r.payload->>'trigger_comment_id')::uuid END AS pcomment
    FROM run_input r
    WHERE r.thread_key IS NULL AND r.owner_task_id IS NULL
      AND r.park_reason IS DISTINCT FROM 'legacy_unowned'
), ev AS (
    SELECT b.seq, car.id AS carrier_id,
           (car.status IN ('queued', 'dispatched', 'running', 'deferred')) AS carrier_active,
           CASE WHEN car.id IS NOT NULL THEN COALESCE(car.comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid)
                WHEN cm.id IS NOT NULL THEN comment_thread_root_id(cm.id) END AS tk
    FROM base b
    LEFT JOIN agent_task_queue car ON car.id = b.ptask AND car.issue_id = b.issue_id AND car.agent_id = b.agent_id
    LEFT JOIN comment cm ON cm.id = b.pcomment AND cm.issue_id = b.issue_id
)
UPDATE run_input r
   SET thread_key = ev.tk,
       state = CASE WHEN r.state = 'held' THEN CASE WHEN COALESCE(ev.carrier_active, false) THEN 'owned' ELSE 'parked' END
                    ELSE r.state END,
       owner_task_id = CASE WHEN r.state = 'held' AND COALESCE(ev.carrier_active, false) THEN ev.carrier_id END,
       park_reason = CASE WHEN r.state = 'held' AND NOT COALESCE(ev.carrier_active, false) THEN 'legacy_unowned'
                          ELSE r.park_reason END
  FROM ev
 WHERE r.seq = ev.seq;

-- Statement 3: legacy claimed (and already parked) batches. A row stays only
-- on task proof that its claimer is the original carrier.
DO $$
DECLARE n record;
BEGIN
    FOR n IN
        SELECT r.state, COALESCE(r.park_reason, '') AS reason, count(*) AS c
          FROM run_input r
         WHERE r.state IN ('claimed', 'parked') AND r.task_id IS NOT NULL
           AND r.park_reason IS DISTINCT FROM 'legacy_unowned'
           AND r.park_reason IS DISTINCT FROM 'legacy_claim_unproven'
           AND NOT EXISTS (SELECT 1 FROM agent_task_queue t
                            WHERE t.id = r.task_id AND t.issue_id = r.issue_id AND t.agent_id = r.agent_id
                              AND r.payload->>'task_id' = r.task_id::text)
         GROUP BY 1, 2
    LOOP
        RAISE NOTICE 'run_input quarantine: state=% previous park_reason=% rows=%', n.state, n.reason, n.c;
    END LOOP;
END $$;

UPDATE run_input r
   SET state = 'parked', park_reason = 'legacy_claim_unproven', owner_task_id = NULL
 WHERE r.state IN ('claimed', 'parked')
   AND r.task_id IS NOT NULL
   AND r.park_reason IS DISTINCT FROM 'legacy_unowned'
   AND r.park_reason IS DISTINCT FROM 'legacy_claim_unproven'
   AND NOT EXISTS (SELECT 1 FROM agent_task_queue t
                    WHERE t.id = r.task_id
                      AND t.issue_id = r.issue_id AND t.agent_id = r.agent_id
                      AND r.payload->>'task_id' = r.task_id::text);

-- I1: an owned row has an owner and a thread. Validated after the backfill.
ALTER TABLE run_input ADD CONSTRAINT run_input_owned_has_owner
    CHECK (state <> 'owned' OR (owner_task_id IS NOT NULL AND thread_key IS NOT NULL)) NOT VALID;
ALTER TABLE run_input VALIDATE CONSTRAINT run_input_owned_has_owner;

-- The coalescing trigger records an owned input: the carrier it fired for.
CREATE OR REPLACE FUNCTION run_input_from_coalesced_comments() RETURNS trigger AS $$
BEGIN
    IF NEW.status <> 'queued' OR NEW.issue_id IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO run_input (workspace_id, issue_id, agent_id, event_id, kind, payload, state, owner_task_id, thread_key)
    SELECT a.workspace_id, NEW.issue_id, NEW.agent_id, 'comment:' || c.id::text, 'comment',
           jsonb_build_object('comment_id', c.id::text, 'task_id', NEW.id::text,
                              'content', left(COALESCE(cm.content, ''), 2000)),
           'owned', NEW.id, COALESCE(NEW.comment_thread_id, '00000000-0000-0000-0000-000000000000'::uuid)
    FROM agent a,
         unnest(NEW.coalesced_comment_ids || CASE WHEN NEW.trigger_comment_id IS NULL THEN '{}'::uuid[] ELSE ARRAY[NEW.trigger_comment_id] END) AS c(id)
         LEFT JOIN comment cm ON cm.id = c.id
    WHERE a.id = NEW.agent_id
    ON CONFLICT (issue_id, agent_id, event_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- I2: no row is ever `held` again. Old code writes `held`; the row is parked
-- for a member instead, so old code is inert. Never raises.
CREATE OR REPLACE FUNCTION run_input_retire_held() RETURNS trigger AS $$
BEGIN
    IF NEW.state = 'held' THEN
        NEW.state := 'parked';
        NEW.park_reason := 'legacy_unowned';
        NEW.owner_task_id := NULL;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER run_input_retire_held
BEFORE INSERT OR UPDATE ON run_input
FOR EACH ROW EXECUTE FUNCTION run_input_retire_held();
