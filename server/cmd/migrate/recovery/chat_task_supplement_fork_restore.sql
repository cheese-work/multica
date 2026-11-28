-- Hand-run recovery for the 99zzz00 chat_task_supplement fork conversion (CHE-10692).
-- Not a migration: the runner never applies this file. Run it only by route 2 of
-- the deploy plan, with an owner decision for each use:
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
--   3. Maps each post-deploy upstream row onto the fork shape, per message.
--   4. Writes chat_task_supplement_restore_report: one row per supplement id.
--   5. Deletes the 99zzz00 row of schema_contracts when that table exists, and the
--      ledger rows of 99zzz00 to 99zzz04.
--
-- Status mapping for an upstream row onto the fork shape:
--   delivered -> delivered (never reverted to pending once delivered)
--   pending, requeued -> pending
--   delivering -> uncertain (claimed, no positive ack)
--   withdrawn -> failed (never claimed; the previous image does not deliver it)
--
-- Per-message rule: the fork table keys rows by (message_id, task_id), but the
-- upstream table holds one row per message. So a message never ends with two
-- rows the previous image would deliver:
--   - if any fork row of the message is delivered, no other row of it stays pending or delivering;
--   - otherwise the upstream row's task is the only pending or delivering row of the message.
-- A row for a second task of an already delivered message is recorded as failed.

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
    final_st   text;
    final_rsn  text;
    outcome    text;
    fork_now   text;
    delivered_elsewhere boolean;
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
    IF to_regclass('schema_contracts') IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM information_schema.columns
                        WHERE table_name = 'schema_contracts' AND column_name = 'version') THEN
        RAISE EXCEPTION 'schema_contracts has no version column; its layout is unknown to this script';
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
    --    and must fit 63 bytes; a 32-hex attempt suffix is short enough.
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

        SELECT EXISTS (SELECT 1 FROM chat_task_supplement f
                        WHERE f.message_id = cur.message_id AND f.status = 'delivered' AND f.task_id <> cur.task_id)
          INTO delivered_elsewhere;

        SELECT f.status INTO fork_now FROM chat_task_supplement f
         WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id;

        IF FOUND THEN
            IF fork_now = 'delivered' OR fork_now = target THEN
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
        ELSE
            -- A new row. If the message already has a row on another task, it is inserted
            -- as its own row. Pending or uncertain never comes back on a delivered message.
            IF EXISTS (SELECT 1 FROM chat_task_supplement f WHERE f.message_id = cur.message_id) THEN
                outcome := 'inserted_cross_task';
            ELSE
                outcome := 'inserted';
            END IF;
            IF target = 'delivered' THEN
                final_st := 'delivered';
            ELSIF delivered_elsewhere THEN
                final_st := 'failed';
            ELSE
                final_st := target;
            END IF;
            final_rsn := CASE
                WHEN final_st = 'failed' AND target = 'failed' THEN 'withdrawn after deploy'
                WHEN final_st = 'failed' THEN 'message already delivered on another task'
            END;
            INSERT INTO chat_task_supplement (message_id, task_id, workspace_id, chat_session_id, author_id,
                                              status, failure_reason, created_at, updated_at, delivered_at)
            VALUES (cur.message_id, cur.task_id, cur.workspace_id, cur.chat_session_id, cur.author_id,
                    final_st, final_rsn, cur.created_at, now(),
                    CASE WHEN final_st = 'delivered' THEN now() END);
        END IF;

        -- Per-message rule. A delivered row on the message supersedes every pending or
        -- delivering row of it. Otherwise only this row's task may stay pending or delivering.
        UPDATE chat_task_supplement f
           SET status = 'failed', failure_reason = 'superseded after deploy', updated_at = now()
         WHERE f.message_id = cur.message_id AND f.status IN ('pending', 'delivering')
           AND (f.task_id <> cur.task_id
                OR EXISTS (SELECT 1 FROM chat_task_supplement d
                            WHERE d.message_id = cur.message_id AND d.status = 'delivered'));

        INSERT INTO chat_task_supplement_restore_report (attempt_id, message_id, task_id, snapshot_status, upstream_status, fork_status, outcome)
        SELECT a, cur.message_id, cur.task_id,
               (SELECT s.status FROM _snap s WHERE s.message_id = cur.message_id AND s.task_id = cur.task_id),
               cur.status,
               (SELECT f.status FROM chat_task_supplement f WHERE f.message_id = cur.message_id AND f.task_id = cur.task_id),
               outcome;
    END LOOP;

    -- Fork rows the upstream table does not hold are kept. Rows the per-message rule
    -- superseded are labelled as such.
    INSERT INTO chat_task_supplement_restore_report (attempt_id, message_id, task_id, snapshot_status, upstream_status, fork_status, outcome)
    SELECT a, f.message_id, f.task_id, s.status, NULL, f.status,
           CASE WHEN f.failure_reason = 'superseded after deploy' THEN 'fork_row_superseded' ELSE 'fork_only' END
      FROM chat_task_supplement f
      LEFT JOIN _snap s ON s.message_id = f.message_id AND s.task_id = f.task_id
     WHERE NOT EXISTS (SELECT 1 FROM _up u WHERE u.message_id = f.message_id AND u.task_id = f.task_id);

    -- 4. Contract ledger and migration ledger.
    IF to_regclass('schema_contracts') IS NOT NULL THEN
        EXECUTE 'DELETE FROM schema_contracts WHERE version = $1' USING '99zzz00_chat_task_supplement_fork_convert';
    END IF;
    DELETE FROM schema_migrations
     WHERE version IN ('99zzz00_chat_task_supplement_fork_convert', '99zzz01_chat_task_supplement',
                       '99zzz02_chat_task_supplement_message', '99zzz03_chat_task_supplement_task',
                       '99zzz04_chat_task_supplement_workspace');

    RAISE NOTICE 'attempt %: % upstream rows saved to %; fork restored to chat_task_supplement; report in chat_task_supplement_restore_report',
        a, upstream_n, saved;
END
$restore$;

COMMIT;
