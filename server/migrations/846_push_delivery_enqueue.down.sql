DROP TRIGGER IF EXISTS trg_inbox_item_push_reply ON inbox_item;
DROP TRIGGER IF EXISTS trg_inbox_item_push_insert ON inbox_item;
DROP FUNCTION IF EXISTS enqueue_push_delivery();
