-- CHE-1795: logout ends a session by writing one marker row, not by updating
-- the device row, so it never waits behind a send in flight. The worker skips
-- devices registered by a session with a marker, and registration refuses an
-- ended session. Rows are never removed by age: sliding renewal keeps a
-- session's `sid` alive for as long as it is used, so no lifetime bounds it.
-- The primary key on session_id is the only index. No foreign keys.
CREATE TABLE IF NOT EXISTS push_session_end (
    session_id text PRIMARY KEY,
    ended_at timestamptz NOT NULL DEFAULT now()
);
