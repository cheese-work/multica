DROP TABLE IF EXISTS ci_flake_increment;

ALTER TABLE system1_observation
    DROP COLUMN IF EXISTS labeled_at,
    DROP COLUMN IF EXISTS time_to_green_ms,
    DROP COLUMN IF EXISTS outcome_label;
