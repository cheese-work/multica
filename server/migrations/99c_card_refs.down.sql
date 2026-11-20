SET LOCAL lock_timeout = '2s';

DROP INDEX IF EXISTS idx_card_ref_pick_pending;
DROP TABLE IF EXISTS card_ref;
