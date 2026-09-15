-- One row per (parent_issue_id, stage, generation) in stage_completion_wake:
-- see migration 475 for the full rationale. Kept as its own single-statement
-- migration because CREATE INDEX CONCURRENTLY cannot run inside a transaction
-- or alongside other statements.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_one_wake_per_parent_stage_generation
    ON stage_completion_wake (parent_issue_id, stage, generation);
