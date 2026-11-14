-- CHE-5594 (slice D of CHE-5046): receipt-level facts a merge_eligibility rule
-- reads without an agent or a network call. One row per PR head and base: a new
-- head or a retarget has no row, so the PR is held until fresh facts are
-- recorded for it. A NULL
-- boolean means the fact is absent, which the evaluator treats as a refusal.
-- A receipt does not expire by age. The trigger below deletes it on every
-- change that can revoke it, in the same transaction as that change, so a
-- retarget round trip (main -> release -> main on one head) or a review
-- withdrawn after the receipt cannot bring the old row back.
CREATE TABLE merge_receipt_fact (
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    head_sha text NOT NULL,
    base_ref text NOT NULL,
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
    PRIMARY KEY (workspace_id, repo_owner, repo_name, pr_number, head_sha, base_ref)
);

CREATE FUNCTION merge_receipt_invalidate() RETURNS trigger AS $$
BEGIN
    DELETE FROM merge_receipt_fact
     WHERE workspace_id = NEW.workspace_id AND repo_owner = NEW.repo_owner
       AND repo_name = NEW.repo_name AND pr_number = NEW.pr_number;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

-- pr_updated_at moves on a label or title change; the review columns move on a
-- review, a request for changes or a dismissal.
CREATE TRIGGER merge_receipt_invalidate AFTER UPDATE ON github_pull_request
 FOR EACH ROW WHEN (OLD.head_sha IS DISTINCT FROM NEW.head_sha
   OR OLD.snapshot_head_sha IS DISTINCT FROM NEW.snapshot_head_sha
   OR OLD.snapshot_base_ref IS DISTINCT FROM NEW.snapshot_base_ref
   OR OLD.state IS DISTINCT FROM NEW.state
   OR OLD.review_state IS DISTINCT FROM NEW.review_state
   OR OLD.review_id IS DISTINCT FROM NEW.review_id
   OR OLD.review_head_sha IS DISTINCT FROM NEW.review_head_sha
   OR OLD.pr_updated_at IS DISTINCT FROM NEW.pr_updated_at)
 EXECUTE FUNCTION merge_receipt_invalidate();
