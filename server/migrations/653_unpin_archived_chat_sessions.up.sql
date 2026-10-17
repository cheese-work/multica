-- Addendum A2 (CHE-1801): an archived DM is never pinned. Clears pins that
-- archived rows still carry; the handlers keep the invariant from here on.
UPDATE chat_session SET pinned_at = NULL WHERE status = 'archived' AND pinned_at IS NOT NULL;
