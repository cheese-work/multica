SET LOCAL lock_timeout = '2s';

-- ci_event rows cannot survive the old kind check.
DELETE FROM system1_observation WHERE kind = 'ci_event';
ALTER TABLE system1_observation DROP CONSTRAINT IF EXISTS system1_observation_kind_check;
ALTER TABLE system1_observation ADD CONSTRAINT system1_observation_kind_check
    CHECK (kind IN ('decision', 'routing')) NOT VALID;
ALTER TABLE system1_observation VALIDATE CONSTRAINT system1_observation_kind_check;

DROP TABLE IF EXISTS gh_budget_reservation;
DROP TABLE IF EXISTS gh_installation_budget;
DROP TABLE IF EXISTS ci_signature_owner;
DROP TABLE IF EXISTS ci_watch_action;
DROP TABLE IF EXISTS ci_failure_event;
DROP TABLE IF EXISTS ci_watch_config_audit;
DROP TABLE IF EXISTS ci_watch_config;
