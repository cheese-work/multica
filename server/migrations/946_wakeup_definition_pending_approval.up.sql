-- An agent may propose a workspace or project wakeup rule (CHE-1082 L13b). The
-- definition is stored disabled and pending_approval marks it until a human
-- admin enables it: resolution keeps a pending definition's rules disabled in
-- every scope below it, so an issue override cannot switch the proposal on.
ALTER TABLE issue_wakeup_definition
 ADD COLUMN IF NOT EXISTS pending_approval boolean NOT NULL DEFAULT false;
