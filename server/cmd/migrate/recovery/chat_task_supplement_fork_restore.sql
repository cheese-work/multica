-- Hand-run recovery for the 99zzz00 chat_task_supplement fork conversion (CHE-10692).
-- Not a migration: the runner never applies this file. Run it only by route 2 of
-- the deploy plan, with a Cheese decision for each use:
--   psql -v ON_ERROR_STOP=1 -v attempt_id=<32 lowercase hex> -f chat_task_supplement_fork_restore.sql
--
-- Needs the pre-deploy snapshot table chat_task_supplement_snapshot_<attempt_id>
-- (the fork shape, taken before the deploy). Its rows must equal
-- chat_task_supplement_fork_875, or the script stops.
--
-- What it does, in one transaction under ACCESS EXCLUSIVE locks:
--   1. Saves the upstream table as chat_task_supplement_upstream_saved_<A> and
--      renames its indexes. Nothing is dropped.
--   2. Puts the fork table back under its fork names from chat_task_supplement_fork_875.
--   3. Maps each post-deploy upstream row onto the fork shape so the previous
--      image neither loses nor re-delivers a supplement. Rows the fork shape cannot
--      express are recorded and left in the saved table.
--   4. Writes chat_task_supplement_restore_report: one row per supplement id.
--   5. Deletes the ledger rows of 99zzz00 to 99zzz04.
--
-- Status mapping for an upstream row onto the fork shape:
--   delivered -> delivered (never reverted to pending once delivered)
--   pending, requeued -> pending
--   delivering -> uncertain (claimed, no positive ack)
--   withdrawn -> failed (never claimed; the previous image does not deliver it)

BEGIN;
SET LOCAL lock_timeout = '5s';
SELECT set_config('recovery.attempt_id', :'attempt_id', true);

DO $restore$
DECLARE
    a          text := current_setting('recovery.attempt_id');
    snap       text;
    saved      text;
    mismatch   bigint;
    cur        record;
    target     text;
    outcome    text;
    upstream_n bigint;
BEGIN
    IF a IS NULL OR a !~ '^[0-9a-f]{32}$' THEN
        RAISE EXCEPTION 'attempt_id must be 32 lowercase hex characters, got %', a;
    END IF;
    snap  := 'chat_task_supplement_snapshot_' || a;
    saved := 'chat_task_supplement_upstream_saved_' || a;

    IF to_regclass(saved) IS NOT NULL THEN
        RAISE EXCEPTION 'attempt % was already restored: % exists', a, saved;
    END IF;
    IF to_regclass('chat_task_supplement_fork_875') IS NULL THEN
        RAISE EXCEPTION 'chat_task_supplement_fork_875 is missing: no fork table to restore from';
    END IF;
    IF to_regclass(format('%I', snap)) IS NULL THEN
        RAISE EXCEPTION 'snapshot table % is missing', snap;
    END IF;
    IF to_regclass('schema_contracts') IS NOT NULL THEN
        RAISE EXCEPTION 'schema_contracts exists here; its row layout is unknown to this script. Stop and report.';
    END IF;

    LOCK TABLE chat_task_supplement IN ACCESS EXCLUSIVE MODE;
    LOCK TABLE chat_task_supplement_fork_875 IN ACCESS EXCLUSIVE MODE;

    -- The snapshot must equal the fork table, in both directions.
    EXECUTE format('SELECT count(*) FROM (SELECT * FROM %I EXCEPT SELECT * FROM chat_task_supplement_fork_875) d', snap)
        INTO mismatch;
    IF mismatch <> 0 THEN
        RAISE EXCEPTION 'snapshot % has % rows missing from chat_task_supplement_fork_875', snap, mismatch;
    END IF;
    EXECUTE format('SELECT count(*) FROM (SELECT * FROM chat_task_supplement_fork_875 EXCEPT SELECT * FROM %I) d', snap)
        INTO mismatch;
    IF mismatch <> 0 THEN
        RAISE EXCEPTION 'chat_task_supplement_fork_875 has % rows missing from snapshot %', mismatch, snap;
    END IF;

    EXECUTE format('CREATE TEMP TABLE _snap ON COMMIT DROP AS SELECT * FROM %I', snap);
    CREATE TEMP TABLE _up ON COMMIT DROP AS SELECT * FROM chat_task_supplement;
    SELECT count(*) INTO upstream_n FROM _up;

    CREATE TABLE IF NOT EXISTS chat_task_supplement_restore_report (
        attempt_id      TEXT NOT NULL,
        message_id      UUID NOT NULL,
        task_id         UUID NOT NULL,
        snapshot_status TEXT,
        upstream_status TEXT,
        fork_status     TEXT,
        outcome         TEXT NOT NULL,
        created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
        PRIMARY KEY (attempt_id, message_id, task_id)
    );

    -- 1. Save the upstream table and its indexes. Index names are schema-global
    --    and short, so they stay under 63 bytes.
    EXECUTE format('ALTER TABLE chat_task_supplement RENAME TO %I', saved);
    IF to_regclass('uq_chat_task_supplement_message') IS NOT NULL THEN
        EXECUTE format('ALTER INDEX uq_chat_task_supplement_message RENAME TO %I', 'uq_cts_saved_msg_' || a);
    END IF;
    IF to_regclass('idx_chat_task_supplement_task') IS NOT NULL THEN
        EXECUTE format('ALTER INDEX idx_chat_task_supplement_task RENAME TO %I', 'idx_cts_saved_task_' || a);
    END IF;
    IF to_regclass('idx_chat_task_supplement_workspace') IS NOT NULL THEN
        EXECUTE format('ALTER INDEX idx_chat_task_supplement_workspace RENAME TO %I', 'idx_cts_saved_ws_' || a);
    END IF;

    -- 2. Restore the fork table under its fork names.
    ALTER TABLE chat_task_supplement_fork_875 RENAME TO chat_task_supplement;
    ALTER INDEX chat_task_supplement_fork_875_pkey RENAME TO chat_task_supplement_pkey;
    ALTER INDEX chat_task_supplement_fork_875_task_status_idx RENAME TO chat_task_supplement_task_status_idx;

    -- 3. Map each post-deploy upstream row onto the fork shape.
    FOR cur IN SELECT * FROM _up LOOP
        target := CASE cur.status
            WHEN 'delivered'  THEN 'delivered'
            WHEN 'pending'    THEN 'pending'
            WHEN 'requeued'   THEN 'pending'
            WHEN 'delivering' THEN 'uncertain'
            WHEN 'withdrawn'  THEN 'failed'
        END;

        IF EXISTS (SELECT 1 FROM chat_task_supplement f WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id) THEN
            IF (SELECT f.status FROM chat_task_supplement f WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id) = 'delivered'
               OR (SELECT f.status FROM chat_task_supplement f WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id) = target THEN
                outcome := 'fork_row_kept';
            ELSE
                UPDATE chat_task_supplement f
                   SET status = target,
                       delivered_at = CASE WHEN target = 'delivered' THEN now() ELSE f.delivered_at END,
                       failure_reason = CASE WHEN target = 'failed' THEN 'withdrawn after deploy' ELSE f.failure_reason END,
                       updated_at = now()
                 WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id;
                outcome := 'fork_row_updated';
            END IF;
        ELSIF EXISTS (SELECT 1 FROM chat_task_supplement f WHERE f.message_id = cur.message_id) THEN
            outcome := 'not_expressible_other_task';
        ELSE
            INSERT INTO chat_task_supplement (message_id, task_id, workspace_id, chat_session_id, author_id,
                                              status, failure_reason, created_at, updated_at, delivered_at)
            VALUES (cur.message_id, cur.task_id, cur.workspace_id, cur.chat_session_id, cur.author_id,
                    target,
                    CASE WHEN target = 'failed' THEN 'withdrawn after deploy' END,
                    cur.created_at, now(),
                    CASE WHEN target = 'delivered' THEN now() END);
            outcome := 'inserted';
        END IF;

        INSERT INTO chat_task_supplement_restore_report (attempt_id, message_id, task_id, snapshot_status, upstream_status, fork_status, outcome)
        SELECT a, cur.message_id, cur.task_id,
               (SELECT s.status FROM _snap s WHERE s.message_id = cur.message_id AND s.task_id = cur.task_id),
               cur.status,
               (SELECT f.status FROM chat_task_supplement f WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id),
               outcome;
    END LOOP;

    -- Fork rows the upstream table does not hold are kept as they are.
    INSERT INTO chat_task_supplement_restore_report (attempt_id, message_id, task_id, snapshot_status, upstream_status, fork_status, outcome)
    SELECT a, f.message_id, f.task_id, s.status, NULL, f.status, 'fork_only'
      FROM chat_task_supplement f
      LEFT JOIN _snap s ON s.message_id = f.message_id AND s.task_id = f.task_id
     WHERE NOT EXISTS (SELECT 1 FROM _up u WHERE u.message_id = f.message_id AND u.task_id = f.task_id);

    -- 4. Ledger: 99zzz00 to 99zzz04 are no longer applied, so a later up run applies them again.
    DELETE FROM schema_migrations
     WHERE version IN ('99zzz00_chat_task_supplement_fork_convert', '99zzz01_chat_task_supplement',
                       '99zzz02_chat_task_supplement_message', '99zzz03_chat_task_supplement_task',
                       '99zzz04_chat_task_supplement_workspace');

    RAISE NOTICE 'attempt %: upstream rows saved to % (%); fork rows restored to chat_task_supplement; report rows written to chat_task_supplement_restore_report',
        a, saved, upstream_n;
END
$restore$;

COMMIT;
