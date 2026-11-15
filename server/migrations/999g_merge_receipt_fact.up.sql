-- CHE-5594 (slice D of CHE-5046): receipt-level facts a merge_eligibility rule
-- reads without an agent or a network call. One row per PR head and base: a new
-- head or a retarget has no row, so the PR is held until fresh facts are
-- recorded for it. A NULL
-- boolean means the fact is absent, which the evaluator treats as a refusal.
-- The cross-lab PASS and its time are not stored here: the server derives them
-- from signed verdict comments and agent records when it reads the facts.
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
    scope_classified boolean,
    gate_vetoed boolean,
    paths text[],
    labels text[],
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

-- A veto Cheese records in Multica only (no GitHub review or comment). It holds
-- the PR until it is cleared, whatever receipt a producer writes meanwhile.
CREATE TABLE merge_veto (
    workspace_id uuid NOT NULL,
    repo_owner text NOT NULL,
    repo_name text NOT NULL,
    pr_number integer NOT NULL,
    vetoed_by uuid NOT NULL,
    reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    PRIMARY KEY (workspace_id, repo_owner, repo_name, pr_number)
);

-- When a comment's text last changed. updated_at also moves on a resolve or an
-- unresolve, so it cannot order verdicts. The merge PASS derivation reads this
-- time (else created_at) as the moment a verdict took effect. No foreign key:
-- a row for a comment that no longer exists is never read.
CREATE TABLE comment_content_edit (
    comment_id uuid PRIMARY KEY,
    edited_at timestamptz NOT NULL
);

CREATE FUNCTION comment_content_edit_record() RETURNS trigger AS $$
BEGIN
    INSERT INTO comment_content_edit(comment_id, edited_at) VALUES (NEW.id, now())
    ON CONFLICT (comment_id) DO UPDATE SET edited_at = EXCLUDED.edited_at;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER comment_content_edit_record AFTER UPDATE OF content ON comment
 FOR EACH ROW WHEN (OLD.content IS DISTINCT FROM NEW.content)
 EXECUTE FUNCTION comment_content_edit_record();
