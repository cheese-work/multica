SET LOCAL lock_timeout = '2s';

-- route rows cannot survive the old kind check.
DELETE FROM system1_observation WHERE kind = 'route';
ALTER TABLE system1_observation DROP CONSTRAINT IF EXISTS system1_observation_kind_check;
ALTER TABLE system1_observation ADD CONSTRAINT system1_observation_kind_check
    CHECK (kind IN ('decision', 'routing', 'ci_event')) NOT VALID;
ALTER TABLE system1_observation VALIDATE CONSTRAINT system1_observation_kind_check;
ALTER TABLE system1_observation DROP COLUMN IF EXISTS event_id;
