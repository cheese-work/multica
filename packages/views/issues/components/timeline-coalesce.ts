/**
 * Timeline activity actions that coalesce differently from the default (the
 * same actor and action within 2 minutes fold into one row with a count).
 * Kept in this small module so the Go contract test can read them without the
 * large issue detail file joining the backend CI scope (CHE-6603 K9).
 */
export const NO_TIME_LIMIT_ACTIONS = new Set(["task_completed", "task_failed", "wakeup_checkin"]);

// Duplicate marks name a different issue on every row.
export const NEVER_COALESCE_ACTIONS = new Set([
  "squad_leader_evaluated",
  "wakeup_created",
  "wakeup_triggered",
  "wakeup_timed_out",
  "wakeup_paused",
  "duplicate_marked",
  "duplicate_unmarked",
  "duplicate_added",
  "duplicate_removed",
]);
