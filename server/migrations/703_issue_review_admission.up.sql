-- The first run created for an issue in in_review on each PR head is its
-- review. The run budget admits that run even when the budget is spent
-- (CHE-2226), so the admission is recorded by the same statement that creates
-- the run: a rolled-back or deduplicated insert never consumes it.
CREATE TABLE issue_review_admission (
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    pull_request_id UUID NOT NULL,
    head_sha TEXT NOT NULL,
    task_id UUID NOT NULL,
    admitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, pull_request_id, head_sha)
);

CREATE FUNCTION reserve_issue_review_admission() RETURNS trigger AS $$
BEGIN
    INSERT INTO issue_review_admission (workspace_id, issue_id, pull_request_id, head_sha, task_id)
    SELECT i.workspace_id, NEW.issue_id, pr.id, pr.head_sha, NEW.id
    FROM issue i
    JOIN issue_pull_request ipr ON ipr.issue_id = i.id
    JOIN github_pull_request pr ON pr.id = ipr.pull_request_id
    WHERE i.id = NEW.issue_id AND i.status = 'in_review'
      AND pr.state IN ('open', 'draft') AND pr.head_sha <> ''
    ON CONFLICT DO NOTHING;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER reserve_issue_review_admission
    AFTER INSERT ON agent_task_queue
    FOR EACH ROW WHEN (NEW.issue_id IS NOT NULL)
    EXECUTE FUNCTION reserve_issue_review_admission();
