-- Reverse of 99zzz00_chat_wakeup_fork_convert: rebuild the fork's chat_wakeup
-- (701, 702, 796, 797, 980) with the rows the up file saved, undo its message
-- marks, and refuse unless the rebuilt table has the digest the up file
-- recorded. The runner rolls back 99zzz15 first (it drops the upstream table
-- and chat_message.wakeup_id), so a down through the train reaches this file
-- with no upstream chat_wakeup.
--
-- Rows written after the up file ran (new upstream wakeups, edits) are not part
-- of the saved copy: this file restores the table as the up file found it. It
-- refuses while an upstream chat_wakeup still holds rows. chat_wakeup_fork_
-- conversion and the digest function stay as an audit record; the saved rows of
-- the reversed conversion are deleted once restored.

DO $restore$
DECLARE
    conv chat_wakeup_fork_conversion%ROWTYPE;
    n INTEGER;
    got TEXT;
BEGIN
    SELECT * INTO conv FROM chat_wakeup_fork_conversion ORDER BY id DESC LIMIT 1;
    IF NOT FOUND THEN
        RAISE EXCEPTION '99zzz00 down: chat_wakeup_fork_conversion has no record; nothing says what to restore';
    END IF;

    IF to_regclass('chat_wakeup') IS NOT NULL THEN
        IF EXISTS (
            SELECT 1 FROM pg_attribute
             WHERE attrelid = 'chat_wakeup'::regclass AND attname = 'created_by_task_id' AND NOT attisdropped
        ) THEN
            RAISE EXCEPTION '99zzz00 down: chat_wakeup already has the fork shape; refusing to restore over it';
        END IF;
        EXECUTE 'SELECT count(*) FROM chat_wakeup' INTO n;
        IF n > 0 THEN
            RAISE EXCEPTION '99zzz00 down: upstream chat_wakeup holds % rows; refusing to drop them', n;
        END IF;
        DROP TABLE chat_wakeup;
    END IF;

    -- The fork table as 701, 796 and 980 leave it, with 702's and 797's indexes.
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

    INSERT INTO chat_wakeup (
        id, workspace_id, chat_session_id, agent_id, instruction, fire_at, status, created_by_task_id,
        fired_task_id, created_at, resolved_at, title, issue_id, event_type, filter_actor_type,
        filter_actor_id, until_status, on_timeout)
    SELECT id, workspace_id, chat_session_id, agent_id, instruction, fire_at, status, created_by_task_id,
           fired_task_id, created_at, resolved_at, title, issue_id, event_type, filter_actor_type,
           filter_actor_id, until_status, on_timeout
      FROM chat_wakeup_fork_saved
     WHERE conversion_id = conv.id;

    got := chat_wakeup_fork_digest('chat_wakeup'::regclass);
    IF got IS DISTINCT FROM conv.fork_digest THEN
        RAISE EXCEPTION '99zzz00 down: restored chat_wakeup digest % differs from the recorded % (conversion %)',
            got, conv.fork_digest, conv.id;
    END IF;

    UPDATE chat_message cm
       SET message_kind = si.message_kind
      FROM chat_wakeup_fork_saved_input si
     WHERE si.conversion_id = conv.id AND si.message_id = cm.id;

    DELETE FROM chat_wakeup_fork_saved WHERE conversion_id = conv.id;
    DELETE FROM chat_wakeup_fork_saved_input WHERE conversion_id = conv.id;

    RAISE NOTICE 'migration report: chat_wakeup fork restore: conversion=% restored_rows=%', conv.id, conv.fork_rows;
END
$restore$;

ALTER TABLE chat_message DROP COLUMN IF EXISTS wakeup_id;
