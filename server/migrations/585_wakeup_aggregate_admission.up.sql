-- Aggregate admission (CHE-1082 L7): starts/hour budgets shared by every
-- instance below a scope. A budget row is the lock a start takes, keyed by
-- (workspace, cap-owning scope, rule key); its identity is the unique index
-- added by 586. A reservation is one counted paid start of one task against one
-- budget (identity: 587, window scan: 588). Whether it still counts is read from
-- the task itself, so a run cancelled or failed before it started gives its slot
-- back with no release step to lose. An issue_wakeup that a full budget delayed
-- names it, says since when it waits (its place in line) and when to look
-- again (589 lists them); 591 serves the retention sweep of old reservations. No primary key,
-- foreign key or cascade; nothing here is read until a cap resolves.
CREATE TABLE IF NOT EXISTS wakeup_aggregate_budget (
 workspace_id uuid NOT NULL,
 scope_kind text NOT NULL,
 scope_id uuid NOT NULL,
 rule_key text NOT NULL,
 starts_per_hour integer NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 CONSTRAINT wakeup_aggregate_budget_scope_kind_check CHECK (scope_kind IN ('workspace','project','issue')),
 CONSTRAINT wakeup_aggregate_budget_limit_check CHECK (starts_per_hour >= 1)
);
CREATE TABLE IF NOT EXISTS wakeup_aggregate_reservation (
 workspace_id uuid NOT NULL,
 scope_kind text NOT NULL,
 scope_id uuid NOT NULL,
 rule_key text NOT NULL,
 task_id uuid NOT NULL,
 wakeup_id uuid NOT NULL,
 reserved_at timestamptz NOT NULL
);
ALTER TABLE issue_wakeup
 ADD COLUMN IF NOT EXISTS aggregate_blocked_scope_kind text,
 ADD COLUMN IF NOT EXISTS aggregate_blocked_scope_id uuid,
 ADD COLUMN IF NOT EXISTS aggregate_blocked_since timestamptz,
 ADD COLUMN IF NOT EXISTS aggregate_retry_at timestamptz;
-- The runner records a version after running its SQL, so an interrupted run
-- executes this file again: the constraint is added only when absent. NOT VALID
-- skips the table scan (and its long lock); every existing row has these columns
-- NULL, which the constraint accepts, and it binds all later writes.
DO $$
BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='issue_wakeup'::regclass AND conname='issue_wakeup_aggregate_blocked_check') THEN
  ALTER TABLE issue_wakeup ADD CONSTRAINT issue_wakeup_aggregate_blocked_check CHECK (
   (aggregate_blocked_scope_kind IS NULL) = (aggregate_blocked_scope_id IS NULL)
   AND (aggregate_blocked_scope_id IS NULL) = (aggregate_retry_at IS NULL)
   AND (aggregate_retry_at IS NULL) = (aggregate_blocked_since IS NULL)
   AND (aggregate_blocked_scope_kind IS NULL OR aggregate_blocked_scope_kind IN ('workspace','project','issue'))) NOT VALID;
 END IF;
END $$;
