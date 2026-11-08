SET LOCAL lock_timeout = '2s';

-- CHE-4439 S1: kind `route` (the human's pick of a status route) and the id of
-- the status_changed activity row it labels. NOT VALID then VALIDATE keeps the
-- table writable while the widened check is verified.
ALTER TABLE system1_observation ADD COLUMN IF NOT EXISTS event_id uuid;
ALTER TABLE system1_observation DROP CONSTRAINT IF EXISTS system1_observation_kind_check;
ALTER TABLE system1_observation ADD CONSTRAINT system1_observation_kind_check
    CHECK (kind IN ('decision', 'routing', 'ci_event', 'route')) NOT VALID;
ALTER TABLE system1_observation VALIDATE CONSTRAINT system1_observation_kind_check;
