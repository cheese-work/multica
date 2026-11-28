-- Restore R for the chat_wakeup fork conversion (CHE-7486 N4a, CHE-10691).
--
-- NOT a migration: the runner never applies this file. People run it by hand,
-- only by route 2 of README "Contract migration recovery", each time with a
-- Cheese decision. It is the `restore=` target of the contract line on
-- 99zzz00_chat_wakeup_fork_convert (X).
--
-- Goal: after X has run, the previous image can serve chat wakeups again. No
-- wakeup fires twice. No wakeup runs an instruction, a time or a condition
-- that a person replaced after the deploy. No deleted wakeup comes back. Every
-- row written after the deploy stays readable (in the saved table), and every
-- row R cannot express is recorded in chat_wakeup_restore_report.
--
-- Run (one transaction; the file holds its own BEGIN and COMMIT):
--
--   PGOPTIONS='-c multica.restore_attempt=<A>' \
--     psql "$DATABASE_URL" -X -v ON_ERROR_STOP=1 -f chat_wakeup_fork_restore.sql
--
-- <A> is the attempt id of X's schema_contracts row: 32 lower-case hexadecimal
-- characters. The setting is optional when schema_contracts holds X's row with
-- an attempt id; when both are given they must match.
--
-- Input (loaded by hand, because the N4b loader is not on main). The snapshot
-- is the controller's `COPY chat_wakeup TO STDOUT` text copy of the fork table
-- from before X (N5a: <state-dir>/contract-snapshots/<A>/chat_wakeup.copy).
-- Load it, and its row in the meta table, before this file:
--
--   CREATE TABLE chat_wakeup_fork_snapshot_<A> (
--       id uuid, workspace_id uuid, chat_session_id uuid, agent_id uuid,
--       instruction text, fire_at timestamptz, status text,
--       created_by_task_id uuid, fired_task_id uuid, created_at timestamptz,
--       resolved_at timestamptz, title text, issue_id uuid, event_type text,
--       filter_actor_type text, filter_actor_id uuid, until_status text,
--       on_timeout text);
--   \copy chat_wakeup_fork_snapshot_<A> FROM '<state-dir>/contract-snapshots/<A>/chat_wakeup.copy'
--   CREATE TABLE IF NOT EXISTS chat_wakeup_fork_snapshot_meta (
--       attempt_id text PRIMARY KEY, rows bigint NOT NULL, sha256 text NOT NULL);
--   INSERT INTO chat_wakeup_fork_snapshot_meta (attempt_id, rows, sha256)
--       VALUES ('<A>', <rows from manifest.json>, '<sha256 from manifest.json>');
--
-- R checks the snapshot's 18 columns and that the meta row's count equals the
-- table's row count. It cannot recompute the SHA-256 (the .copy file is not in
-- the database): whoever loads the snapshot checks it against manifest.json.
--
-- Ledger names on fork main, as they map to the contract's "X and 675-678":
--   X                            99zzz00_chat_wakeup_fork_convert
--   X's sqlc hint (CHE-9434)     99zzz00_chat_wakeup_fork_convert_sqlc_hint
--   675 (the upstream table)     99zzz15_chat_wakeup
--   676 (due index)              99zzz16_chat_wakeup_due_idx
--   677 (session index)          99zzz17_chat_wakeup_session_idx
--   678 (requester index)        99zzz18_chat_wakeup_requester_idx
-- R deletes these six ledger rows and X's schema_contracts row (when the table
-- exists). Every other ledger row stays. chat_wakeup_fork_conversion and the
-- X audit tables stay. The previous image ignores the extra schema.
--
-- What R does, in order, holding ACCESS EXCLUSIVE on chat_wakeup:
--   1. Renames the upstream table to chat_wakeup_upstream_saved_<A> and its
--      primary key, indexes and constraints to cwus_<A>_<n> (n by name order).
--      No fixed name, so a later restore cannot collide.
--   2. Creates the fork-shaped chat_wakeup (701, 796, 980) with its indexes.
--   3. Fills it by id (rules below) and writes chat_wakeup_restore_report:
--      one row per snapshot id and per saved id, plus one row with the nil id
--      and outcome 'saved_table_renames' whose detail lists every old and new
--      name. Earlier attempts' rows stay.
--   4. Deletes the ledger rows above and X's schema_contracts row.
--   5. RAISE NOTICE the count of each outcome.
--
-- Notation: S is the snapshot row, U the saved upstream row with the same id,
-- X(S) the mapping of S by X. "U is unchanged" means U's title, instruction,
-- fire_at, expires_at and the condition keys type, issue_id, status,
-- author_type, author_id and since equal those of X(S). next_eval_at,
-- condition_state and the display keys identifier and author_name are ignored:
-- the dispatcher and the display code write them.
--
-- "The fork can express U": kind at; condition issue with status; condition
-- issue_reply by a member or an agent. A reply condition with no author_type
-- is a person's edit (the upstream service drops author_type) and upstream
-- evaluates it as "a member replies", so it counts as member. Everything else
-- cannot be expressed: check, children_done, pull_request, run, issue with
-- state, issue_reply with author_type any (a fork watch with no filter would
-- also wake on system and plugin comments).
--
--   S         U                                   fork row R writes             outcome
--   pending   active, unchanged                   S as it was                   restored_pending
--   pending   active, changed, expressible        from U (see below)            from_current
--   pending   active, changed, not expressible    S, cancelled, resolved now()  ended_not_expressible
--   pending   ended after the deploy              fired (U.last_task_id) or     ended_after_deploy
--                                                 cancelled; title and
--                                                 instruction from U
--   terminal  present                             S as it was                   restored_terminal
--   any       absent                              no row, never revived         absent
--   none      expressible                         from U (ended U as above)     converted_new
--   none      not expressible                     no row; U stays saved         not_restored
--
-- "From U": title, instruction; at: fire_at = U.fire_at; issue: issue_id and
-- until_status; issue_reply: event_type comment.created, the actor filter from
-- author_type and author_id, created_at = since (the fork's comment window
-- starts at created_at); a wait gets fire_at = U.expires_at and on_timeout
-- 'wake'. id and created_by_task_id come from S (from U.source_task_id for
-- converted_new).
--
-- Counts: restored_pending + from_current + ended_not_expressible +
-- ended_after_deploy + restored_terminal + absent = snapshot rows;
-- converted_new + not_restored = saved rows whose id is not in the snapshot.
-- The saved table, the snapshot table and A's report rows are kept.
--
-- Not covered by the contract: chat_message rows that X marked as wakeup
-- inputs (message_kind 'wakeup', wakeup_id) stay marked.

BEGIN;

-- The safe casts return NULL for text that is not a uuid or a timestamp, so a
-- malformed condition counts as "cannot be expressed" instead of aborting R.
CREATE FUNCTION pg_temp.restore_uuid(v text) RETURNS uuid
LANGUAGE plpgsql IMMUTABLE AS $f$
BEGIN
    RETURN v::uuid;
EXCEPTION WHEN others THEN
    RETURN NULL;
END
$f$;

CREATE FUNCTION pg_temp.restore_ts(v text) RETURNS timestamptz
LANGUAGE plpgsql STABLE AS $f$
BEGIN
    RETURN v::timestamptz;
EXCEPTION WHEN others THEN
    RETURN NULL;
END
$f$;

DO $restore$
DECLARE
    x_version CONSTANT TEXT := '99zzz00_chat_wakeup_fork_convert';
    ledger_versions CONSTANT TEXT[] := ARRAY[
        '99zzz00_chat_wakeup_fork_convert',
        '99zzz00_chat_wakeup_fork_convert_sqlc_hint',
        '99zzz15_chat_wakeup',
        '99zzz16_chat_wakeup_due_idx',
        '99zzz17_chat_wakeup_session_idx',
        '99zzz18_chat_wakeup_requester_idx'];
    a TEXT;
    contract_a TEXT;
    snap TEXT;
    saved TEXT;
    meta_rows BIGINT;
    snap_rows BIGINT;
    saved_rows BIGINT;
    renames JSONB := '[]'::jsonb;
    obj RECORD;
    n INTEGER := 0;
    new_name TEXT;
    x_applied BOOLEAN;
    deleted_ledger INTEGER;
    counts TEXT;
    snap_side BIGINT;
    new_side BIGINT;
    want_cols CONSTANT TEXT := 'id uuid, workspace_id uuid, chat_session_id uuid, agent_id uuid, instruction text, '
        'fire_at timestamp with time zone, status text, created_by_task_id uuid, fired_task_id uuid, '
        'created_at timestamp with time zone, resolved_at timestamp with time zone, title text, issue_id uuid, '
        'event_type text, filter_actor_type text, filter_actor_id uuid, until_status text, on_timeout text';
    got_cols TEXT;
BEGIN
    -- Lock first: nothing below may see a row change.
    IF to_regclass('chat_wakeup') IS NULL THEN
        RAISE EXCEPTION 'restore: table chat_wakeup does not exist';
    END IF;
    LOCK TABLE chat_wakeup IN ACCESS EXCLUSIVE MODE;

    -- Attempt id A.
    a := nullif(current_setting('multica.restore_attempt', true), '');
    IF to_regclass('schema_contracts') IS NOT NULL THEN
        EXECUTE 'SELECT attempt_id FROM schema_contracts WHERE version = $1' INTO contract_a USING x_version;
    END IF;
    IF a IS NOT NULL AND contract_a IS NOT NULL AND a <> contract_a THEN
        RAISE EXCEPTION 'restore: multica.restore_attempt % differs from the attempt id % in the schema_contracts row of %',
            a, contract_a, x_version;
    END IF;
    a := coalesce(a, contract_a);
    IF a IS NULL THEN
        RAISE EXCEPTION 'restore: no attempt id; set multica.restore_attempt or keep the schema_contracts row of %', x_version;
    END IF;
    IF a !~ '^[0-9a-f]{32}$' THEN
        RAISE EXCEPTION 'restore: attempt id % is not 32 lower-case hexadecimal characters', a;
    END IF;
    snap := format('%I', 'chat_wakeup_fork_snapshot_' || a);
    saved := format('%I', 'chat_wakeup_upstream_saved_' || a);

    -- X must have run, and the live table must be X's output.
    EXECUTE 'SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)' INTO x_applied USING x_version;
    IF NOT x_applied THEN
        RAISE EXCEPTION 'restore: % has no schema_migrations row; there is nothing to restore', x_version;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'chat_wakeup'::regclass AND attname = 'kind' AND NOT attisdropped)
       OR EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid = 'chat_wakeup'::regclass AND attname = 'created_by_task_id' AND NOT attisdropped) THEN
        RAISE EXCEPTION 'restore: chat_wakeup does not have the upstream shape; refusing';
    END IF;
    IF to_regclass(saved) IS NOT NULL THEN
        RAISE EXCEPTION 'restore: % already exists; attempt % was restored before', saved, a;
    END IF;

    -- The snapshot: the table, its 18 fork columns, its meta row and count.
    IF to_regclass(snap) IS NULL THEN
        RAISE EXCEPTION 'restore: snapshot table % does not exist', snap;
    END IF;
    SELECT string_agg(attname || ' ' || format_type(atttypid, atttypmod), ', ' ORDER BY attnum) INTO got_cols
      FROM pg_attribute WHERE attrelid = to_regclass(snap) AND attnum > 0 AND NOT attisdropped;
    IF got_cols IS DISTINCT FROM want_cols THEN
        RAISE EXCEPTION 'restore: snapshot table % does not have the fork chat_wakeup columns: %', snap, got_cols;
    END IF;
    IF to_regclass('chat_wakeup_fork_snapshot_meta') IS NULL THEN
        RAISE EXCEPTION 'restore: table chat_wakeup_fork_snapshot_meta does not exist';
    END IF;
    EXECUTE 'SELECT rows FROM chat_wakeup_fork_snapshot_meta WHERE attempt_id = $1' INTO meta_rows USING a;
    IF meta_rows IS NULL THEN
        RAISE EXCEPTION 'restore: chat_wakeup_fork_snapshot_meta has no row for attempt %', a;
    END IF;
    EXECUTE 'SELECT count(*) FROM ' || snap INTO snap_rows;
    IF snap_rows <> meta_rows THEN
        RAISE EXCEPTION 'restore: snapshot table % holds % rows but its meta row says %', snap, snap_rows, meta_rows;
    END IF;

    -- 1. Rename the upstream table, its constraints and its indexes.
    EXECUTE 'SELECT count(*) FROM chat_wakeup' INTO saved_rows;
    EXECUTE format('ALTER TABLE chat_wakeup RENAME TO %I', 'chat_wakeup_upstream_saved_' || a);
    renames := renames || jsonb_build_array(jsonb_build_object('kind', 'table', 'from', 'chat_wakeup', 'to', 'chat_wakeup_upstream_saved_' || a));
    FOR obj IN
        SELECT kind, name FROM (
            SELECT 'constraint' AS kind, conname::text AS name
              FROM pg_constraint WHERE conrelid = to_regclass(saved) AND contype IN ('p', 'u', 'c', 'x')
            UNION ALL
            SELECT 'index', c.relname::text
              FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
             WHERE i.indrelid = to_regclass(saved)
               AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid AND k.conrelid = i.indrelid)
        ) o ORDER BY name
    LOOP
        n := n + 1;
        new_name := format('cwus_%s_%s', a, n);
        IF obj.kind = 'constraint' THEN
            EXECUTE format('ALTER TABLE %s RENAME CONSTRAINT %I TO %I', saved, obj.name, new_name);
        ELSE
            EXECUTE format('ALTER INDEX %I RENAME TO %I', obj.name, new_name);
        END IF;
        renames := renames || jsonb_build_array(jsonb_build_object('kind', obj.kind, 'from', obj.name, 'to', new_name));
    END LOOP;

    -- 2. The fork table as 701, 796 and 980 leave it, with 702's and 797's indexes.
    CREATE TABLE chat_wakeup (
        id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
        workspace_id UUID NOT NULL,
        chat_session_id UUID NOT NULL,
        agent_id UUID NOT NULL,
        instruction TEXT NOT NULL,
        fire_at TIMESTAMPTZ NOT NULL,
        status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'fired', 'cancelled')),
        created_by_task_id UUID,
        fired_task_id UUID,
        created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        resolved_at TIMESTAMPTZ,
        title TEXT NOT NULL DEFAULT '',
        issue_id UUID,
        event_type TEXT NOT NULL DEFAULT '',
        filter_actor_type TEXT NOT NULL DEFAULT '',
        filter_actor_id UUID,
        until_status TEXT NOT NULL DEFAULT '',
        on_timeout TEXT NOT NULL DEFAULT 'drop'
    );
    CREATE INDEX idx_chat_wakeup_pending ON chat_wakeup (fire_at) WHERE status = 'pending';
    CREATE INDEX idx_chat_wakeup_fired_task ON chat_wakeup (fired_task_id) WHERE fired_task_id IS NOT NULL;
    -- Held to the end of the transaction like the lock on the renamed table.
    LOCK TABLE chat_wakeup IN ACCESS EXCLUSIVE MODE;

    CREATE TABLE IF NOT EXISTS chat_wakeup_restore_report (
        attempt_id TEXT NOT NULL,
        id UUID NOT NULL,
        outcome TEXT NOT NULL,
        session_exists BOOLEAN NOT NULL,
        detail JSONB NOT NULL DEFAULT '{}'::jsonb,
        restored_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        PRIMARY KEY (attempt_id, id)
    );

    -- 3. Classify every id of the snapshot and of the saved table, then fill.
    EXECUTE format($plan$
        CREATE TEMP TABLE chat_wakeup_restore_plan ON COMMIT DROP AS
        WITH s AS (
            SELECT * FROM %1$s
        ), u AS (
            SELECT * FROM %2$s
        ), xs AS (  -- X(S): what X made of each snapshot row
            SELECT s.id,
                   CASE WHEN w.is_watch THEN 'condition' ELSE 'at' END AS kind,
                   CASE WHEN s.title = '' THEN left(s.instruction, 120) ELSE s.title END AS title,
                   s.instruction,
                   CASE WHEN w.is_watch THEN NULL ELSE s.fire_at END AS fire_at,
                   s.fire_at AS expires_at,
                   CASE WHEN NOT w.is_watch THEN NULL WHEN s.until_status <> '' THEN 'issue' ELSE 'issue_reply' END AS ctype,
                   CASE WHEN w.is_watch THEN s.issue_id::text END AS c_issue_id,
                   CASE WHEN w.is_watch AND s.until_status <> '' THEN s.until_status END AS c_status,
                   CASE WHEN w.is_watch AND s.until_status = '' THEN
                        CASE WHEN s.filter_actor_type IN ('member', 'agent') THEN s.filter_actor_type ELSE 'any' END END AS c_author_type,
                   CASE WHEN w.is_watch AND s.until_status = '' THEN s.filter_actor_id::text END AS c_author_id,
                   CASE WHEN w.is_watch AND s.until_status = '' THEN
                        rtrim(rtrim(to_char(s.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), '0'), '.') || 'Z' END AS c_since
              FROM s
              CROSS JOIN LATERAL (SELECT (s.issue_id IS NOT NULL AND (s.until_status <> '' OR s.event_type = 'comment.created')) AS is_watch) w
        ), ux AS (  -- what the fork could express of U, and its parts
            SELECT u.id,
                   u.condition->>'type' AS ctype,
                   u.condition->>'status' AS c_status,
                   u.condition->>'state' AS c_state,
                   u.condition->>'author_type' AS c_author_type,
                   u.condition->>'author_id' AS c_author_id,
                   u.condition->>'since' AS c_since,
                   u.condition->>'issue_id' AS c_issue_id,
                   CASE
                       WHEN u.kind = 'at' THEN true
                       WHEN u.kind <> 'condition' THEN false
                       WHEN u.condition->>'type' = 'issue' THEN
                            coalesce(u.condition->>'status', '') <> '' AND u.condition->>'state' IS NULL
                            AND pg_temp.restore_uuid(u.condition->>'issue_id') IS NOT NULL
                       WHEN u.condition->>'type' = 'issue_reply' THEN
                            coalesce(u.condition->>'author_type', 'member') IN ('member', 'agent')
                            AND pg_temp.restore_uuid(u.condition->>'issue_id') IS NOT NULL
                            AND pg_temp.restore_ts(u.condition->>'since') IS NOT NULL
                            AND (u.condition->>'author_id' IS NULL OR pg_temp.restore_uuid(u.condition->>'author_id') IS NOT NULL)
                       ELSE false
                   END AS expressible
              FROM u
        ), j AS (
            SELECT coalesce(s.id, u.id) AS id,
                   s.id IS NOT NULL AS in_s, u.id IS NOT NULL AS in_u,
                   s.workspace_id AS s_workspace_id, s.chat_session_id AS s_chat_session_id, s.agent_id AS s_agent_id,
                   s.instruction AS s_instruction, s.fire_at AS s_fire_at, s.status AS s_status,
                   s.created_by_task_id AS s_created_by_task_id, s.fired_task_id AS s_fired_task_id,
                   s.created_at AS s_created_at, s.resolved_at AS s_resolved_at, s.title AS s_title,
                   s.issue_id AS s_issue_id, s.event_type AS s_event_type, s.filter_actor_type AS s_filter_actor_type,
                   s.filter_actor_id AS s_filter_actor_id, s.until_status AS s_until_status, s.on_timeout AS s_on_timeout,
                   u.workspace_id AS u_workspace_id, u.chat_session_id AS u_chat_session_id, u.agent_id AS u_agent_id,
                   u.source_task_id AS u_source_task_id, u.title AS u_title, u.instruction AS u_instruction,
                   u.kind AS u_kind, u.fire_at AS u_fire_at, u.expires_at AS u_expires_at, u.status AS u_status,
                   u.ended_at AS u_ended_at, u.last_task_id AS u_last_task_id, u.last_fired_at AS u_last_fired_at,
                   u.created_at AS u_created_at,
                   ux.ctype AS u_ctype, ux.c_status AS u_c_status, ux.c_state AS u_c_state,
                   ux.c_author_type AS u_c_author_type, ux.c_author_id AS u_c_author_id,
                   ux.c_since AS u_c_since, ux.c_issue_id AS u_c_issue_id, coalesce(ux.expressible, false) AS u_expressible,
                   -- U is unchanged: the contract's field list, nothing else
                   (u.id IS NOT NULL AND s.id IS NOT NULL
                    AND u.title IS NOT DISTINCT FROM xs.title
                    AND u.instruction IS NOT DISTINCT FROM xs.instruction
                    AND u.fire_at IS NOT DISTINCT FROM xs.fire_at
                    AND u.expires_at IS NOT DISTINCT FROM xs.expires_at
                    AND ux.ctype IS NOT DISTINCT FROM xs.ctype
                    AND ux.c_issue_id IS NOT DISTINCT FROM xs.c_issue_id
                    AND ux.c_status IS NOT DISTINCT FROM xs.c_status
                    AND ux.c_author_type IS NOT DISTINCT FROM xs.c_author_type
                    AND ux.c_author_id IS NOT DISTINCT FROM xs.c_author_id
                    AND ux.c_since IS NOT DISTINCT FROM xs.c_since) AS u_unchanged
              FROM s
              FULL JOIN u ON u.id = s.id
              LEFT JOIN xs ON xs.id = s.id
              LEFT JOIN ux ON ux.id = u.id
        ), o AS (
            SELECT j.*,
                   CASE
                       WHEN in_s AND NOT in_u THEN 'absent'
                       WHEN in_s AND s_status <> 'pending' THEN 'restored_terminal'
                       WHEN in_s AND u_status <> 'active' THEN 'ended_after_deploy'
                       WHEN in_s AND u_unchanged THEN 'restored_pending'
                       WHEN in_s AND u_expressible THEN 'from_current'
                       WHEN in_s THEN 'ended_not_expressible'
                       WHEN u_expressible THEN 'converted_new'
                       ELSE 'not_restored'
                   END AS outcome
              FROM j
        )
        SELECT o.*,
               EXISTS (SELECT 1 FROM chat_session cs WHERE cs.id = coalesce(o.s_chat_session_id, o.u_chat_session_id)) AS session_exists
          FROM o
    $plan$, snap, saved);

    -- The rows of the new table. A column that has a "from U" value reads U;
    -- a row from the snapshot alone reads S; the ended outcomes override
    -- status, task and time.
    INSERT INTO chat_wakeup (
        id, workspace_id, chat_session_id, agent_id, instruction, fire_at, status, created_by_task_id,
        fired_task_id, created_at, resolved_at, title, issue_id, event_type, filter_actor_type,
        filter_actor_id, until_status, on_timeout)
    SELECT p.id,
           coalesce(p.s_workspace_id, p.u_workspace_id),
           coalesce(p.s_chat_session_id, p.u_chat_session_id),
           coalesce(p.s_agent_id, p.u_agent_id),
           CASE WHEN p.outcome IN ('from_current', 'converted_new', 'ended_after_deploy') THEN p.u_instruction ELSE p.s_instruction END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN
                   CASE WHEN p.u_kind = 'at' THEN p.u_fire_at ELSE p.u_expires_at END
               ELSE p.s_fire_at
           END,
           CASE
               WHEN p.outcome IN ('restored_pending', 'from_current') THEN 'pending'
               WHEN p.outcome = 'ended_not_expressible' THEN 'cancelled'
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') AND p.u_status = 'active' THEN 'pending'
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') AND p.u_last_task_id IS NOT NULL THEN 'fired'
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') THEN 'cancelled'
               ELSE p.s_status
           END,
           CASE WHEN p.outcome = 'converted_new' THEN p.u_source_task_id ELSE p.s_created_by_task_id END,
           CASE
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') AND p.u_status <> 'active' AND p.u_last_task_id IS NOT NULL
                   THEN p.u_last_task_id
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new', 'ended_not_expressible', 'from_current', 'restored_pending')
                   THEN NULL
               ELSE p.s_fired_task_id
           END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new')
                    AND p.u_ctype = 'issue_reply' THEN pg_temp.restore_ts(p.u_c_since)
               WHEN p.outcome = 'converted_new' THEN p.u_created_at
               ELSE p.s_created_at
           END,
           CASE
               WHEN p.outcome IN ('restored_pending', 'from_current') THEN NULL
               WHEN p.outcome = 'ended_not_expressible' THEN now()
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') AND p.u_status = 'active' THEN NULL
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') AND p.u_last_task_id IS NOT NULL
                   THEN coalesce(p.u_last_fired_at, p.u_ended_at, now())
               WHEN p.outcome IN ('ended_after_deploy', 'converted_new') THEN coalesce(p.u_ended_at, now())
               ELSE p.s_resolved_at
           END,
           CASE WHEN p.outcome IN ('from_current', 'converted_new', 'ended_after_deploy') THEN p.u_title ELSE p.s_title END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN pg_temp.restore_uuid(p.u_c_issue_id)
               ELSE p.s_issue_id
           END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN
                   CASE WHEN p.u_ctype = 'issue_reply' THEN 'comment.created' ELSE '' END
               ELSE p.s_event_type
           END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN
                   CASE WHEN p.u_ctype = 'issue_reply' THEN coalesce(p.u_c_author_type, 'member') ELSE '' END
               ELSE p.s_filter_actor_type
           END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN
                   CASE WHEN p.u_ctype = 'issue_reply' THEN pg_temp.restore_uuid(p.u_c_author_id) END
               ELSE p.s_filter_actor_id
           END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN
                   CASE WHEN p.u_ctype = 'issue' THEN p.u_c_status ELSE '' END
               ELSE p.s_until_status
           END,
           CASE
               WHEN p.outcome IN ('from_current', 'converted_new') THEN
                   CASE WHEN p.u_kind = 'at' THEN 'drop' ELSE 'wake' END
               ELSE p.s_on_timeout
           END
      FROM chat_wakeup_restore_plan p
     WHERE p.outcome NOT IN ('absent', 'not_restored');

    INSERT INTO chat_wakeup_restore_report (attempt_id, id, outcome, session_exists, detail)
    SELECT a, p.id, p.outcome, p.session_exists,
           jsonb_strip_nulls(jsonb_build_object(
               'snapshot_status', p.s_status,
               'saved_status', p.u_status,
               'saved_kind', p.u_kind,
               'saved_condition_type', p.u_ctype,
               'saved_condition_state', p.u_c_state))
      FROM chat_wakeup_restore_plan p;

    INSERT INTO chat_wakeup_restore_report (attempt_id, id, outcome, session_exists, detail)
    VALUES (a, '00000000-0000-0000-0000-000000000000', 'saved_table_renames', false,
            jsonb_build_object('saved_table', 'chat_wakeup_upstream_saved_' || a,
                               'snapshot_table', 'chat_wakeup_fork_snapshot_' || a,
                               'renames', renames));

    -- The counts must add up before anything is committed.
    EXECUTE 'SELECT count(*) FROM chat_wakeup_restore_plan WHERE in_s' INTO snap_side;
    EXECUTE 'SELECT count(*) FROM chat_wakeup_restore_plan WHERE NOT in_s' INTO new_side;
    IF snap_side <> snap_rows OR new_side <> saved_rows - (SELECT count(*) FROM chat_wakeup_restore_plan WHERE in_s AND in_u) THEN
        RAISE EXCEPTION 'restore: counts do not add up: snapshot side % of % rows, new side % of % saved rows',
            snap_side, snap_rows, new_side, saved_rows;
    END IF;

    -- 4. The previous image's schema proof admits the previous image again, and
    -- a roll-forward runs X and its companions again.
    EXECUTE 'DELETE FROM schema_migrations WHERE version = ANY($1)' USING ledger_versions;
    GET DIAGNOSTICS deleted_ledger = ROW_COUNT;
    IF to_regclass('schema_contracts') IS NOT NULL THEN
        EXECUTE 'DELETE FROM schema_contracts WHERE version = $1' USING x_version;
    END IF;

    -- 5. The count of each outcome.
    SELECT string_agg(outcome || '=' || cnt, ' ' ORDER BY outcome) INTO counts
      FROM (SELECT outcome, count(*) AS cnt FROM chat_wakeup_restore_plan GROUP BY outcome) c;
    RAISE NOTICE 'migration report: chat_wakeup fork restore: attempt=% snapshot_rows=% saved_rows=% ledger_rows_deleted=% %',
        a, snap_rows, saved_rows, deleted_ledger, coalesce(counts, 'no rows');
END
$restore$;

COMMIT;
