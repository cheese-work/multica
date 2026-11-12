-- CHE-2798 S1d: the moment a person's status move stops opening a run-budget
-- window by default. Rows written before it keep the old rule; after it only
-- an interactive session's ui_or_api move (and a top-up) opens one.
CREATE TABLE run_budget_window_cutover (
    only_row   boolean PRIMARY KEY DEFAULT true CHECK (only_row),
    cutover_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO run_budget_window_cutover DEFAULT VALUES;
