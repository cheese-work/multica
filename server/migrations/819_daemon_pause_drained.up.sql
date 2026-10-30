-- CHE-2673: set once the paused machine reached 0 running tasks and its owner
-- was told. A new pause (the upsert) clears it, so each pause notifies once.
ALTER TABLE daemon_pause ADD COLUMN drained_notified_at TIMESTAMPTZ;
