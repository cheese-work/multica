-- The first run for an issue's assignee in in_review on each PR head is its
-- review. The run budget admits that run even when the budget is spent
-- (CHE-2226). The primary key makes the claim atomic: of any number of
-- concurrent enqueues for one head, exactly one inserts the row.
CREATE TABLE issue_review_admission (
    workspace_id UUID NOT NULL,
    issue_id UUID NOT NULL,
    head_sha TEXT NOT NULL,
    agent_id UUID NOT NULL,
    admitted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, head_sha)
);
