-- CHE-5594 (slice D of CHE-5046): receipt-level facts a merge_eligibility rule
-- reads without an agent or a network call. One row per PR head: a new head has
-- no row, so the PR is held until fresh facts are recorded for it. A NULL
-- boolean means the fact is absent, which the evaluator treats as a refusal.
CREATE TABLE merge_receipt_fact (
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    head_sha text NOT NULL,
    tier integer,
    classifier_trusted boolean,
    coverage_complete boolean,
    pr_bound boolean,
    security_gate text NOT NULL DEFAULT '',
    cross_lab_pass boolean,
    scope_classified boolean,
    gate_vetoed boolean,
    paths text[],
    labels text[],
    last_pass_at timestamptz,
    evidence text NOT NULL,
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, repo_owner, repo_name, pr_number, head_sha)
);
