-- CHE-1795: queue a push for every device of the recipient when an inbox item
-- is inserted, or when a DM record takes a new reply (chat_message_id moves).
-- A trigger rather than a Go helper: many writers create inbox items, some
-- outside any transaction, and a later one would otherwise silently skip the
-- outbox. The item and its delivery rows commit or roll back together.
--
-- A DM-linked item (chat_session_id set) follows the `dm` push switch, every
-- other item the `inbox` switch; a missing row means off. The send is 10 s out
-- so a quick read can cancel it; an update to a pending or sent row re-arms it
-- (the lease column is left alone, the worker compares next_attempt_at).
CREATE OR REPLACE FUNCTION enqueue_push_delivery() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.recipient_type <> 'member' OR NEW.read OR NEW.archived THEN
        RETURN NULL;
    END IF;
    IF NOT COALESCE((
        SELECT p.enabled FROM notification_channel_preference p
         WHERE p.workspace_id = NEW.workspace_id AND p.user_id = NEW.recipient_id
           AND p.channel = 'push'
           AND p.source = CASE WHEN NEW.chat_session_id IS NULL THEN 'inbox' ELSE 'dm' END
    ), false) THEN
        RETURN NULL;
    END IF;
    INSERT INTO push_delivery (record_id, subscription_id, next_attempt_at)
    SELECT NEW.id, s.id, now() + interval '10 seconds'
      FROM push_subscription s
     WHERE s.user_id = NEW.recipient_id
    ON CONFLICT (record_id, subscription_id) DO UPDATE
       SET next_attempt_at = EXCLUDED.next_attempt_at, attempts = 0, sent_at = NULL;
    RETURN NULL;
END
$$;

DROP TRIGGER IF EXISTS trg_inbox_item_push_insert ON inbox_item;
CREATE TRIGGER trg_inbox_item_push_insert
    AFTER INSERT ON inbox_item
    FOR EACH ROW EXECUTE FUNCTION enqueue_push_delivery();

DROP TRIGGER IF EXISTS trg_inbox_item_push_reply ON inbox_item;
CREATE TRIGGER trg_inbox_item_push_reply
    AFTER UPDATE OF chat_message_id ON inbox_item
    FOR EACH ROW WHEN (NEW.chat_message_id IS DISTINCT FROM OLD.chat_message_id)
    EXECUTE FUNCTION enqueue_push_delivery();
