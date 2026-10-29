SET LOCAL lock_timeout = '2s';

ALTER TABLE decision_answer
    DROP COLUMN IF EXISTS answered_label,
    DROP COLUMN IF EXISTS excerpted;
