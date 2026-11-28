-- multica:contract snapshot=chat_wakeup restore=server/cmd/migrate/recovery/chat_wakeup_fork_restore.sql
-- Convert the fork's chat_wakeup (701, 796, 980) to upstream's table (99zzz15)
-- before 99zzz15–99zzz18 run. 99zzz15 then finds the table and the column and
-- skips them; 99zzz16–99zzz18 build their indexes on the converted table.
--
-- Holds no transaction control: the runner applies the file and its ledger row
-- in one transaction. Every fork row is converted or counted as dropped; the
-- NOTICE reports the three counts and chat_wakeup_fork_conversion records the
-- fork row count and digest as they were before any change.
--
-- The down file is a real reverse. Before it changes anything, this file copies
-- every fork row, orphans included, to chat_wakeup_fork_saved and the prior
-- message_kind of every fired input row to chat_wakeup_fork_saved_input, both
-- keyed by the conversion record's id. The down file rebuilds the fork table
-- from them and checks the result against the recorded digest.

CREATE TABLE IF NOT EXISTS chat_wakeup_fork_conversion (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    attempt_id TEXT,
    converted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    fork_rows INTEGER NOT NULL,
    fork_digest TEXT NOT NULL
);

-- md5 of the rows' text forms ordered by id; md5('') for no rows. The function
-- settings pin every session setting that changes a row's text form. It
-- refuses a table that does not carry the fork's 18 columns in order.
CREATE OR REPLACE FUNCTION chat_wakeup_fork_digest(tbl regclass) RETURNS text
LANGUAGE plpgsql
SET TimeZone = 'UTC'
SET DateStyle = 'ISO, YMD'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 3
AS $fn$
DECLARE
    got TEXT;
    want CONSTANT TEXT := 'id uuid, workspace_id uuid, chat_session_id uuid, agent_id uuid, instruction text, '
        'fire_at timestamp with time zone, status text, created_by_task_id uuid, fired_task_id uuid, '
        'created_at timestamp with time zone, resolved_at timestamp with time zone, title text, issue_id uuid, '
        'event_type text, filter_actor_type text, filter_actor_id uuid, until_status text, on_timeout text';
    digest TEXT;
BEGIN
    SELECT string_agg(attname || ' ' || format_type(atttypid, atttypmod), ', ' ORDER BY attnum)
      INTO got
      FROM pg_attribute
     WHERE attrelid = tbl AND attnum > 0 AND NOT attisdropped;
    IF got IS DISTINCT FROM want THEN
        RAISE EXCEPTION 'chat_wakeup_fork_digest: % does not have the fork chat_wakeup columns: %', tbl, got;
    END IF;
    EXECUTE format('SELECT md5(coalesce(string_agg(t::text, E''\n'' ORDER BY t.id), '''')) FROM %s t', tbl) INTO digest;
    RETURN digest;
END
$fn$;

-- The fork rows as they were, every column of the fork table plus the id of
-- the conversion that saved them. No defaults and no foreign keys.
CREATE TABLE IF NOT EXISTS chat_wakeup_fork_saved (
    conversion_id BIGINT NOT NULL,
    id UUID NOT NULL,
    workspace_id UUID NOT NULL,
    chat_session_id UUID NOT NULL,
    agent_id UUID NOT NULL,
    instruction TEXT NOT NULL,
    fire_at TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL,
    created_by_task_id UUID,
    fired_task_id UUID,
    created_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    title TEXT NOT NULL,
    issue_id UUID,
    event_type TEXT NOT NULL,
    filter_actor_type TEXT NOT NULL,
    filter_actor_id UUID,
    until_status TEXT NOT NULL,
    on_timeout TEXT NOT NULL,
    PRIMARY KEY (conversion_id, id)
);

-- message_kind of each chat_message that X may mark as a wakeup input, as it
-- was before X.
CREATE TABLE IF NOT EXISTS chat_wakeup_fork_saved_input (
    conversion_id BIGINT NOT NULL,
    message_id UUID NOT NULL,
    message_kind TEXT NOT NULL,
    PRIMARY KEY (conversion_id, message_id)
);

ALTER TABLE chat_message ADD COLUMN IF NOT EXISTS wakeup_id UUID;

DO $convert$
DECLARE
    fork_exists BOOLEAN;
    fork_rows INTEGER := 0;
    fork_digest TEXT;
    conversion BIGINT;
    n_active INTEGER := 0;
    n_terminal INTEGER := 0;
BEGIN
    -- The fork table is the one that still has created_by_task_id; a table of
    -- upstream's shape (an earlier run, a restore) is left alone.
    SELECT EXISTS (
        SELECT 1 FROM pg_attribute
         WHERE attrelid = to_regclass('chat_wakeup') AND attname = 'created_by_task_id' AND NOT attisdropped
    ) INTO fork_exists;

    IF fork_exists THEN
        EXECUTE 'SELECT count(*) FROM chat_wakeup' INTO fork_rows;
        fork_digest := chat_wakeup_fork_digest('chat_wakeup'::regclass);
    ELSE
        fork_digest := md5('');
    END IF;

    INSERT INTO chat_wakeup_fork_conversion (attempt_id, converted_at, fork_rows, fork_digest)
    VALUES (nullif(current_setting('multica.contract_attempt', true), ''), now(), fork_rows, fork_digest)
    RETURNING id INTO conversion;

    IF fork_exists THEN
        -- Index names are schema-wide: move the fork's aside so the new table
        -- can use them; the table is dropped below, after its rows are saved.
        ALTER TABLE chat_wakeup RENAME TO chat_wakeup_fork_old;
        ALTER INDEX IF EXISTS chat_wakeup_pkey RENAME TO chat_wakeup_fork_old_pkey;
        ALTER INDEX IF EXISTS idx_chat_wakeup_pending RENAME TO chat_wakeup_fork_old_pending;
        ALTER INDEX IF EXISTS idx_chat_wakeup_fired_task RENAME TO chat_wakeup_fork_old_fired_task;
    END IF;

    IF fork_exists THEN
        -- Everything the down file needs, before any row is converted. Rows
        -- with no chat_session are kept here although they convert to nothing.
        INSERT INTO chat_wakeup_fork_saved
        SELECT conversion, w.* FROM chat_wakeup_fork_old w;

        INSERT INTO chat_wakeup_fork_saved_input (conversion_id, message_id, message_kind)
        SELECT DISTINCT conversion, cm.id, cm.message_kind
          FROM chat_wakeup_fork_old w
          JOIN chat_message cm ON cm.role = 'user' AND cm.task_id = w.fired_task_id
                              AND cm.chat_session_id = w.chat_session_id
         WHERE w.status = 'fired';
    END IF;

    -- Exactly 99zzz15's table.
    CREATE TABLE IF NOT EXISTS chat_wakeup (
        id UUID PRIMARY KEY,
        workspace_id UUID NOT NULL,
        chat_session_id UUID NOT NULL,
        agent_id UUID NOT NULL,
        requester_id UUID NOT NULL,
        source_task_id UUID,
        source_message_id UUID,
        title TEXT NOT NULL,
        instruction TEXT NOT NULL,
        kind TEXT NOT NULL CHECK (kind IN ('condition', 'at', 'check')),
        condition JSONB,
        condition_state TEXT NOT NULL DEFAULT '',
        interval_seconds BIGINT,
        fire_at TIMESTAMPTZ,
        next_eval_at TIMESTAMPTZ NOT NULL,
        expires_at TIMESTAMPTZ NOT NULL,
        status TEXT NOT NULL DEFAULT 'active'
            CHECK (status IN ('active', 'fired', 'expired', 'cancelled', 'failed')),
        end_reason TEXT,
        ended_at TIMESTAMPTZ,
        check_count INTEGER NOT NULL DEFAULT 0,
        last_check_note TEXT,
        last_checked_at TIMESTAMPTZ,
        last_checkin_task_id UUID,
        last_task_id UUID,
        last_fired_at TIMESTAMPTZ,
        last_error TEXT,
        created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
        CHECK ((kind = 'condition') = (condition IS NOT NULL)),
        CHECK ((kind = 'at') = (fire_at IS NOT NULL)),
        CHECK ((kind = 'check') = (interval_seconds IS NOT NULL))
    );

    IF fork_exists THEN
        -- A row is a watch when it names an issue and carries one of the two
        -- fork watch shapes (validChatWatch: exactly one of until_status and
        -- event_type); any other row keeps its timer as kind 'at'.
        WITH src AS (
            SELECT w.*,
                   cs.creator_id AS session_creator,
                   (w.issue_id IS NOT NULL AND (w.until_status <> '' OR w.event_type = 'comment.created')) AS is_watch,
                   i.id AS issue_found,
                   ws.issue_prefix || '-' || i.number AS ident,
                   m_user.name AS member_name
              FROM chat_wakeup_fork_old w
              JOIN chat_session cs ON cs.id = w.chat_session_id
              LEFT JOIN issue i ON i.id = w.issue_id AND i.workspace_id = w.workspace_id
              LEFT JOIN workspace ws ON ws.id = i.workspace_id
              LEFT JOIN LATERAL (
                    SELECT u.name FROM member mb JOIN "user" u ON u.id = mb.user_id
                     WHERE w.filter_actor_type = 'member' AND mb.workspace_id = w.workspace_id AND mb.user_id = w.filter_actor_id
                     LIMIT 1) m_user ON true
        ), mapped AS (
            SELECT s.*,
                   coalesce(t.initiator_user_id, t.originator_user_id, s.session_creator) AS requester
              FROM src s
              LEFT JOIN agent_task_queue t ON t.id = s.created_by_task_id
        ), ins AS (
            INSERT INTO chat_wakeup (
                id, workspace_id, chat_session_id, agent_id, requester_id, source_task_id, source_message_id,
                title, instruction, kind, condition, fire_at, next_eval_at, expires_at, status, end_reason,
                ended_at, last_task_id, last_fired_at, created_at, updated_at)
            SELECT m.id, m.workspace_id, m.chat_session_id, m.agent_id, m.requester, m.created_by_task_id, NULL,
                   CASE WHEN m.title = '' THEN left(m.instruction, 120) ELSE m.title END,
                   m.instruction,
                   CASE WHEN m.is_watch THEN 'condition' ELSE 'at' END,
                   CASE
                       WHEN NOT m.is_watch THEN NULL
                       WHEN m.until_status <> '' THEN jsonb_strip_nulls(jsonb_build_object(
                           'type', 'issue', 'issue_id', m.issue_id::text, 'identifier', m.ident, 'status', m.until_status))
                       ELSE jsonb_strip_nulls(jsonb_build_object(
                           'type', 'issue_reply', 'issue_id', m.issue_id::text, 'identifier', m.ident,
                           'since', rtrim(rtrim(to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'), '0'), '.') || 'Z',
                           'author_type', CASE WHEN m.filter_actor_type IN ('member', 'agent') THEN m.filter_actor_type ELSE 'any' END,
                           'author_id', m.filter_actor_id::text,
                           'author_name', m.member_name))
                   END,
                   CASE WHEN m.is_watch THEN NULL ELSE m.fire_at END,
                   CASE
                       WHEN m.status = 'pending' AND m.is_watch THEN now()
                       WHEN m.status = 'pending' THEN m.fire_at
                       ELSE coalesce(m.resolved_at, m.fire_at)
                   END,
                   m.fire_at,
                   CASE m.status WHEN 'pending' THEN 'active' ELSE m.status END,
                   CASE
                       WHEN m.status = 'pending' THEN NULL
                       WHEN m.status = 'fired' AND NOT m.is_watch THEN 'time'
                       ELSE 'fork_migrated'
                   END,
                   CASE WHEN m.status = 'pending' THEN NULL ELSE m.resolved_at END,
                   CASE WHEN m.status = 'fired' THEN m.fired_task_id END,
                   CASE WHEN m.status = 'fired' THEN m.resolved_at END,
                   m.created_at,
                   coalesce(m.resolved_at, m.created_at)
              FROM mapped m
            RETURNING id, status, last_task_id, chat_session_id
        ), inputs AS (  -- data-modifying CTEs run to completion even when unreferenced
            -- The fired input row of a wakeup turn: role user, task_id =
            -- fired_task_id, same session (the fork's match rule).
            UPDATE chat_message cm
               SET wakeup_id = ins.id, message_kind = 'wakeup'
              FROM ins
             WHERE ins.status = 'fired' AND cm.role = 'user'
               AND cm.task_id = ins.last_task_id AND cm.chat_session_id = ins.chat_session_id
            RETURNING cm.id
        )
        SELECT count(*) FILTER (WHERE status = 'active'), count(*) FILTER (WHERE status <> 'active')
          INTO n_active, n_terminal
          FROM ins;

        DROP TABLE chat_wakeup_fork_old;
    END IF;

    RAISE NOTICE 'migration report: chat_wakeup fork conversion: fork_rows=% converted_active=% converted_terminal=% dropped_orphan=%',
        fork_rows, n_active, n_terminal, fork_rows - n_active - n_terminal;
END
$convert$;
