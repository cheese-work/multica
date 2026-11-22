SET LOCAL lock_timeout = '2s';

-- CHE-8749 S7a: the time a run was first dispatched. Set once by the first
-- claim and kept by every requeue and reclaim, which clear or refresh
-- dispatched_at. NULL means the run was never dispatched, or was dispatched
-- before this column existed and has not been reclaimed since. Nullable with
-- no default: a bare ADD COLUMN is metadata-only.
ALTER TABLE agent_task_queue
    ADD COLUMN IF NOT EXISTS first_dispatched_at timestamptz;
